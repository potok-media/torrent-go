package media

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
)

// Thumbnail decodes a single video frame at ~timeSec and returns it as a width×height JPEG — in-process
// (custom AVIO over the torrent cache), replacing the ffmpeg `-ss … -vframes 1 -s … image2` subprocess.
// It seeks to the keyframe at/just before timeSec (good enough for a scrubbing thumbnail), decodes one
// frame, scales it to yuvj420p at the requested size, and MJPEG-encodes it.
func Thumbnail(ctx context.Context, src io.ReadSeeker, timeSec float64, width, height int) ([]byte, error) {
	fc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var vstream *astiav.Stream
	for _, s := range fc.Streams() {
		// Skip cover-art stills (ATTACHED_PIC): thumbnailing a 1-frame image would return the cover, not a
		// frame at the requested time.
		if s.CodecParameters().MediaType() == astiav.MediaTypeVideo && !s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic) {
			vstream = s
			break
		}
	}
	if vstream == nil {
		return nil, fmt.Errorf("media: thumbnail: no video stream")
	}
	videoIdx := vstream.Index()

	decCodec := astiav.FindDecoder(vstream.CodecParameters().CodecID())
	if decCodec == nil {
		return nil, fmt.Errorf("media: thumbnail: no decoder for %s", vstream.CodecParameters().CodecID().String())
	}
	dec := astiav.AllocCodecContext(decCodec)
	if dec == nil {
		return nil, fmt.Errorf("media: thumbnail: alloc decoder")
	}
	defer dec.Free()
	if err := vstream.CodecParameters().ToCodecContext(dec); err != nil {
		return nil, fmt.Errorf("media: thumbnail: decoder params: %w", err)
	}
	if err := dec.Open(decCodec, nil); err != nil {
		return nil, fmt.Errorf("media: thumbnail: open decoder: %w", err)
	}

	tb := vstream.TimeBase()
	if timeSec > 0 && tb.Num() > 0 {
		ts := int64(timeSec * float64(tb.Den()) / float64(tb.Num()))
		if err := fc.SeekFrame(videoIdx, ts, astiav.SeekFlags(astiav.SeekFlagBackward)); err != nil {
			return nil, fmt.Errorf("media: thumbnail: seek: %w", err)
		}
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	frame := astiav.AllocFrame()
	defer frame.Free()

	got := false
	for {
		rerr := fc.ReadFrame(pkt)
		if rerr != nil {
			if errors.Is(rerr, astiav.ErrEof) {
				_ = dec.SendPacket(nil) // flush
				if dec.ReceiveFrame(frame) == nil {
					got = true
				}
				break
			}
			pkt.Unref()
			return nil, fmt.Errorf("media: thumbnail: read frame: %w", rerr)
		}
		if pkt.StreamIndex() != videoIdx {
			pkt.Unref()
			continue
		}
		if err := dec.SendPacket(pkt); err != nil {
			pkt.Unref()
			return nil, fmt.Errorf("media: thumbnail: send packet: %w", err)
		}
		pkt.Unref()
		derr := dec.ReceiveFrame(frame)
		if derr == nil {
			got = true
			break
		}
		if errors.Is(derr, astiav.ErrEagain) {
			continue
		}
		return nil, fmt.Errorf("media: thumbnail: receive frame: %w", derr)
	}
	if !got {
		return nil, fmt.Errorf("media: thumbnail: no frame decoded")
	}

	// Scale to the target size + yuvj420p (what the MJPEG encoder wants).
	sws, err := astiav.CreateSoftwareScaleContext(
		frame.Width(), frame.Height(), frame.PixelFormat(),
		width, height, astiav.PixelFormatYuvj420P,
		astiav.SoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
	)
	if err != nil {
		return nil, fmt.Errorf("media: thumbnail: sws: %w", err)
	}
	defer sws.Free()
	scaled := astiav.AllocFrame()
	defer scaled.Free()
	scaled.SetWidth(width)
	scaled.SetHeight(height)
	scaled.SetPixelFormat(astiav.PixelFormatYuvj420P)
	if err := sws.ScaleFrame(frame, scaled); err != nil {
		return nil, fmt.Errorf("media: thumbnail: scale: %w", err)
	}

	encCodec := astiav.FindEncoder(astiav.CodecIDMjpeg)
	if encCodec == nil {
		return nil, fmt.Errorf("media: thumbnail: no MJPEG encoder")
	}
	enc := astiav.AllocCodecContext(encCodec)
	if enc == nil {
		return nil, fmt.Errorf("media: thumbnail: alloc MJPEG encoder")
	}
	defer enc.Free()
	enc.SetWidth(width)
	enc.SetHeight(height)
	enc.SetPixelFormat(astiav.PixelFormatYuvj420P)
	enc.SetTimeBase(astiav.NewRational(1, 25))
	if err := enc.Open(encCodec, nil); err != nil {
		return nil, fmt.Errorf("media: thumbnail: open MJPEG encoder: %w", err)
	}

	scaled.SetPts(0)
	if err := enc.SendFrame(scaled); err != nil {
		return nil, fmt.Errorf("media: thumbnail: send frame: %w", err)
	}
	_ = enc.SendFrame(nil) // flush
	out := astiav.AllocPacket()
	defer out.Free()
	if err := enc.ReceivePacket(out); err != nil {
		return nil, fmt.Errorf("media: thumbnail: receive packet: %w", err)
	}
	jpeg := make([]byte, len(out.Data()))
	copy(jpeg, out.Data())
	out.Unref()
	return jpeg, nil
}
