package media

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

// audioEncoder transcodes one incompatible source audio stream (AC3/EAC3/DTS/TrueHD/…) to browser-playable
// AAC, in-process: decode → resample to the AAC encoder's sample format → buffer in an AudioFifo →
// emit fixed-size (encoder FrameSize, 1024) AAC frames → mux. The AAC encoder needs exactly FrameSize
// samples per frame, which never matches the decoder's frame size, hence the FIFO reframing.
//
// Timestamps are kept ABSOLUTE (nextPts is seeded from the first decoded frame's pts, in output samples)
// so the transcoded audio stays aligned with the copied/transcoded video across segments (Review R6).
type audioEncoder struct {
	ofc    *astiav.FormatContext
	outIdx int
	w      *fragWriter // shared one-packet-delay writer: stamps exact per-packet durations (gapless segments)

	dec  *astiav.CodecContext
	enc  *astiav.CodecContext
	swr  *astiav.SoftwareResampleContext
	fifo *astiav.AudioFifo

	inTimeBase astiav.Rational
	decFrame   *astiav.Frame
	resampled  *astiav.Frame
	outFrame   *astiav.Frame
	pkt        *astiav.Packet

	started bool
	nextPts int64

	// Segment window in this (audio) stream's timebase — the audio rendition's primary. Decoded frames
	// outside [startTS, endTS) are dropped so the segment's audio starts exactly at its grid boundary (no
	// pre-roll from the backward seek) and doesn't spill into the next segment. Symmetric with videoEncoder.
	startTS int64
	endTS   int64
}

// newAudioEncoder wires the decode→resample→AAC-encode pipeline for input stream srcIdx and adds the AAC
// output stream to ofc (before WriteHeader). Caller must free() it.
func newAudioEncoder(ifc, ofc *astiav.FormatContext, srcIdx int, startTS, endTS int64, fw *fragWriter) (*audioEncoder, error) {
	in := ifc.Streams()[srcIdx]

	decCodec := astiav.FindDecoder(in.CodecParameters().CodecID())
	if decCodec == nil {
		return nil, fmt.Errorf("media: no audio decoder for %s", in.CodecParameters().CodecID().String())
	}
	dec := astiav.AllocCodecContext(decCodec)
	if dec == nil {
		return nil, fmt.Errorf("media: alloc audio decoder")
	}
	if err := in.CodecParameters().ToCodecContext(dec); err != nil {
		dec.Free()
		return nil, fmt.Errorf("media: audio decoder params: %w", err)
	}
	if err := dec.Open(decCodec, nil); err != nil {
		dec.Free()
		return nil, fmt.Errorf("media: open audio decoder: %w", err)
	}

	encCodec := astiav.FindEncoder(astiav.CodecIDAac)
	if encCodec == nil {
		dec.Free()
		return nil, fmt.Errorf("media: no AAC encoder")
	}
	enc := astiav.AllocCodecContext(encCodec)
	if enc == nil {
		dec.Free()
		return nil, fmt.Errorf("media: alloc AAC encoder")
	}
	enc.SetSampleRate(dec.SampleRate())
	// Downmix to stereo: multichannel (5.1 "side" etc.) AAC needs a PCE and trips some browser MSE
	// decoders; stereo is universally safe and matches what the legacy producer forced (-ac 2). The
	// resampler downmixes automatically (source layout → this stereo layout).
	enc.SetChannelLayout(astiav.ChannelLayoutStereo)
	if sfs := encCodec.SampleFormats(); len(sfs) > 0 {
		enc.SetSampleFormat(sfs[0]) // AAC → fltp
	} else {
		enc.SetSampleFormat(dec.SampleFormat())
	}
	enc.SetTimeBase(astiav.NewRational(1, dec.SampleRate()))
	if ofc.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		enc.SetFlags(enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}
	if err := enc.Open(encCodec, nil); err != nil {
		dec.Free()
		enc.Free()
		return nil, fmt.Errorf("media: open AAC encoder: %w", err)
	}

	out := ofc.NewStream(nil)
	if out == nil {
		dec.Free()
		enc.Free()
		return nil, fmt.Errorf("media: new AAC output stream")
	}
	if err := out.CodecParameters().FromCodecContext(enc); err != nil {
		dec.Free()
		enc.Free()
		return nil, fmt.Errorf("media: AAC encoder params -> stream: %w", err)
	}
	out.SetTimeBase(enc.TimeBase())

	a := &audioEncoder{
		ofc:        ofc,
		outIdx:     out.Index(),
		w:          fw,
		dec:        dec,
		enc:        enc,
		swr:        astiav.AllocSoftwareResampleContext(),
		fifo:       astiav.AllocAudioFifo(enc.SampleFormat(), enc.ChannelLayout().Channels(), 1),
		inTimeBase: in.TimeBase(),
		decFrame:   astiav.AllocFrame(),
		resampled:  astiav.AllocFrame(),
		outFrame:   astiav.AllocFrame(),
		pkt:        astiav.AllocPacket(),
		startTS:    startTS,
		endTS:      endTS,
	}
	return a, nil
}

func (a *audioEncoder) free() {
	a.pkt.Free()
	a.outFrame.Free()
	a.resampled.Free()
	a.decFrame.Free()
	a.fifo.Free()
	a.swr.Free()
	a.enc.Free()
	a.dec.Free()
}

// feed decodes one source audio packet, resamples the frames to the encoder format, buffers them, and
// encodes any full frames now available.
func (a *audioEncoder) feed(pkt *astiav.Packet) error {
	if err := a.dec.SendPacket(pkt); err != nil {
		return fmt.Errorf("media: audio send packet: %w", err)
	}
	for {
		err := a.dec.ReceiveFrame(a.decFrame)
		if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: audio receive frame: %w", err)
		}
		// Window trim: drop frames outside [startTS, endTS) so the segment's audio abuts its neighbours
		// instead of carrying the pre-roll the backward seek reads before the grid boundary.
		if pts := a.decFrame.Pts(); pts != noPTS && (pts < a.startTS || pts >= a.endTS) {
			a.decFrame.Unref()
			continue
		}
		if !a.started {
			// Seed the output sample clock from the first IN-WINDOW frame's absolute pts so the segment's
			// audio starts exactly at its grid boundary.
			a.nextPts = int64(float64(a.decFrame.Pts()) * float64(a.inTimeBase.Num()) / float64(a.inTimeBase.Den()) * float64(a.enc.SampleRate()))
			a.started = true
		}

		a.resampled.Unref()
		a.resampled.SetSampleFormat(a.enc.SampleFormat())
		a.resampled.SetChannelLayout(a.enc.ChannelLayout())
		a.resampled.SetSampleRate(a.enc.SampleRate())
		a.resampled.SetNbSamples(a.decFrame.NbSamples())
		if err := a.resampled.AllocBuffer(0); err != nil {
			a.decFrame.Unref()
			return fmt.Errorf("media: audio alloc resample buffer: %w", err)
		}
		if err := a.swr.ConvertFrame(a.decFrame, a.resampled); err != nil {
			a.decFrame.Unref()
			return fmt.Errorf("media: audio resample: %w", err)
		}
		if _, err := a.fifo.Write(a.resampled); err != nil {
			a.decFrame.Unref()
			return fmt.Errorf("media: audio fifo write: %w", err)
		}
		a.decFrame.Unref()

		if err := a.drain(false); err != nil {
			return err
		}
	}
}

// drain pulls full encoder frames out of the FIFO (and, on flush, the final short frame) and encodes them.
func (a *audioEncoder) drain(flush bool) error {
	frameSize := a.enc.FrameSize()
	if frameSize <= 0 {
		frameSize = 1024
	}
	for a.fifo.Size() >= frameSize || (flush && a.fifo.Size() > 0) {
		n := frameSize
		if a.fifo.Size() < n {
			n = a.fifo.Size()
		}
		a.outFrame.Unref()
		a.outFrame.SetSampleFormat(a.enc.SampleFormat())
		a.outFrame.SetChannelLayout(a.enc.ChannelLayout())
		a.outFrame.SetSampleRate(a.enc.SampleRate())
		a.outFrame.SetNbSamples(n)
		if err := a.outFrame.AllocBuffer(0); err != nil {
			return fmt.Errorf("media: audio alloc out buffer: %w", err)
		}
		if _, err := a.fifo.Read(a.outFrame); err != nil {
			return fmt.Errorf("media: audio fifo read: %w", err)
		}
		a.outFrame.SetPts(a.nextPts)
		a.nextPts += int64(n)
		if err := a.encode(a.outFrame); err != nil {
			return err
		}
	}
	return nil
}

// encode sends one frame (nil = flush) to the AAC encoder and writes every packet it emits.
func (a *audioEncoder) encode(f *astiav.Frame) error {
	if err := a.enc.SendFrame(f); err != nil {
		return fmt.Errorf("media: audio send frame: %w", err)
	}
	for {
		err := a.enc.ReceivePacket(a.pkt)
		if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: audio receive packet: %w", err)
		}
		a.pkt.SetStreamIndex(a.outIdx)
		a.pkt.RescaleTs(a.enc.TimeBase(), a.ofc.Streams()[a.outIdx].TimeBase())
		if err := a.w.write(a.pkt); err != nil {
			a.pkt.Unref()
			return err
		}
		a.pkt.Unref()
	}
}

// flush drains the FIFO's remainder and flushes the encoder.
func (a *audioEncoder) flush() error {
	if err := a.drain(true); err != nil {
		return err
	}
	return a.encode(nil)
}
