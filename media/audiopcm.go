package media

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
)

// AudioPCM decodes the first audio stream over [startSec, startSec+durSec], resampled to mono
// signed-16-bit little-endian PCM at `sampleRate` Hz, and returns the interleaved samples — IN-PROCESS
// (custom AVIO over the torrent cache). It replaces the ffmpeg `-ac 1 -ar N -f wav` subprocess that fed
// the intro/outro fingerprinter over the loopback HTTP stream. The bytes are raw PCM (no WAV header);
// the caller wraps them (fpcalc wants a WAV).
func AudioPCM(ctx context.Context, src io.ReadSeeker, startSec, durSec float64, sampleRate int) ([]byte, error) {
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var astream *astiav.Stream
	for _, s := range fc.Streams() {
		if s.CodecParameters().MediaType() == astiav.MediaTypeAudio {
			astream = s
			break
		}
	}
	if astream == nil {
		return nil, fmt.Errorf("media: audiopcm: no audio stream")
	}
	audioIdx := astream.Index()

	decCodec := astiav.FindDecoder(astream.CodecParameters().CodecID())
	if decCodec == nil {
		return nil, fmt.Errorf("media: audiopcm: no decoder for %s", astream.CodecParameters().CodecID().String())
	}
	dec := astiav.AllocCodecContext(decCodec)
	if dec == nil {
		return nil, fmt.Errorf("media: audiopcm: alloc decoder")
	}
	defer dec.Free()
	if err := astream.CodecParameters().ToCodecContext(dec); err != nil {
		return nil, fmt.Errorf("media: audiopcm: decoder params: %w", err)
	}
	if err := dec.Open(decCodec, nil); err != nil {
		return nil, fmt.Errorf("media: audiopcm: open decoder: %w", err)
	}

	swr := astiav.AllocSoftwareResampleContext()
	defer swr.Free()

	tb := astream.TimeBase()
	endSec := startSec + durSec
	if startSec > 0 && tb.Num() > 0 {
		ts := int64(startSec * float64(tb.Den()) / float64(tb.Num()))
		_ = fc.SeekFrame(audioIdx, ts, astiav.SeekFlags(astiav.SeekFlagBackward))
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	decFrame := astiav.AllocFrame()
	defer decFrame.Free()
	outFrame := astiav.AllocFrame()
	defer outFrame.Free()

	var pcm []byte
	resampled := false

	// appendResampled converts one decoded frame (or, with in==nil, flushes the resampler's buffered tail)
	// to mono S16 at sampleRate and appends the interleaved bytes. swr auto-initializes from the first
	// frame's params and updates outFrame.NbSamples() to the actual count it wrote.
	appendResampled := func(in *astiav.Frame) error {
		var inN int64
		if in != nil {
			inN = int64(in.NbSamples())
		} else if !resampled {
			return nil // nothing was ever fed → nothing to flush
		}
		outFrame.Unref()
		outFrame.SetSampleFormat(astiav.SampleFormatS16)
		outFrame.SetChannelLayout(astiav.ChannelLayoutMono)
		outFrame.SetSampleRate(sampleRate)
		// Capacity: buffered delay (in output samples) + this frame's samples + slack. Downmix/downsample
		// only ever yields <= inN output samples, so this never underflows; swr corrects nb_samples down.
		outFrame.SetNbSamples(int(swr.Delay(int64(sampleRate)) + inN + 1024))
		if err := outFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("media: audiopcm: alloc out buffer: %w", err)
		}
		if err := swr.ConvertFrame(in, outFrame); err != nil {
			return fmt.Errorf("media: audiopcm: resample: %w", err)
		}
		resampled = true
		if outFrame.NbSamples() <= 0 {
			return nil
		}
		sz, err := outFrame.SamplesBufferSize(1)
		if err != nil {
			return fmt.Errorf("media: audiopcm: sample size: %w", err)
		}
		buf := make([]byte, sz)
		if _, err := outFrame.SamplesCopyToBuffer(buf, 1); err != nil {
			return fmt.Errorf("media: audiopcm: copy samples: %w", err)
		}
		pcm = append(pcm, buf...)
		return nil
	}

	done := false
	for !done {
		rerr := fc.ReadFrame(pkt)
		if rerr != nil {
			if !errors.Is(rerr, astiav.ErrEof) {
				pkt.Unref()
				return nil, fmt.Errorf("media: audiopcm: read: %w", rerr)
			}
			_ = dec.SendPacket(nil) // EOF → flush the decoder, then drain its remaining frames below
		} else if pkt.StreamIndex() != audioIdx {
			pkt.Unref()
			continue
		} else {
			if err := dec.SendPacket(pkt); err != nil {
				pkt.Unref()
				return nil, fmt.Errorf("media: audiopcm: send packet: %w", err)
			}
			pkt.Unref()
		}

		for {
			derr := dec.ReceiveFrame(decFrame)
			if errors.Is(derr, astiav.ErrEagain) {
				break
			}
			if errors.Is(derr, astiav.ErrEof) {
				done = true
				break
			}
			if derr != nil {
				return nil, fmt.Errorf("media: audiopcm: receive frame: %w", derr)
			}
			if tb.Den() > 0 && decFrame.Pts() != astiav.NoPtsValue {
				if psec := float64(decFrame.Pts()) * float64(tb.Num()) / float64(tb.Den()); psec >= endSec {
					decFrame.Unref()
					done = true
					break
				}
			}
			if err := appendResampled(decFrame); err != nil {
				decFrame.Unref()
				return nil, err
			}
			decFrame.Unref()
		}
	}

	// Flush the resampler's internal buffer.
	if err := appendResampled(nil); err != nil {
		return nil, err
	}
	return pcm, nil
}
