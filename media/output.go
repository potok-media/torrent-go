package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
)

// memWriter is a seekable in-memory sink for the fMP4 muxer. The mov/mp4 muxer seeks back to patch box
// sizes even in fragmented mode, so a plain bytes.Buffer isn't enough — we back it with a growable slice
// plus a cursor. All muxer output for one segment fits comfortably in RAM (a few hundred KB–few MB).
type memWriter struct {
	buf []byte
	pos int64
}

func (m *memWriter) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		m.buf = append(m.buf, make([]byte, end-int64(len(m.buf)))...)
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memWriter) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = m.pos + offset
	case io.SeekEnd:
		np = int64(len(m.buf)) + offset
	default:
		return 0, errors.New("media: bad whence")
	}
	if np < 0 {
		return 0, errors.New("media: negative position")
	}
	m.pos = np
	return np, nil
}

// Bytes returns the full muxed output written so far.
func (m *memWriter) Bytes() []byte { return m.buf }

// openOutput creates an in-memory fragmented-MP4 muxer that writes to mw through a custom AVIO — no temp
// file, no ffmpeg subprocess. Caller adds streams, then WriteHeader → WriteInterleavedFrame… → WriteTrailer.
// Returns the output FormatContext and a cleanup func the caller MUST defer.
func openOutput(mw *memWriter) (*astiav.FormatContext, func(), error) {
	ofc, err := astiav.AllocOutputFormatContext(nil, "mp4", "")
	if err != nil {
		return nil, nil, fmt.Errorf("media: alloc output format context: %w", err)
	}
	if ofc == nil {
		return nil, nil, fmt.Errorf("media: alloc output format context: nil")
	}

	// Writable AVIO: no reader, seek + write over the in-memory buffer.
	ioCtx, err := astiav.AllocIOContext(
		avioBufferSize,
		true,
		nil,
		func(offset int64, whence int) (int64, error) { return mw.Seek(offset, whence) },
		func(b []byte) (int, error) { return mw.Write(b) },
	)
	if err != nil {
		ofc.Free()
		return nil, nil, fmt.Errorf("media: alloc output io context: %w", err)
	}
	ofc.SetPb(ioCtx)

	cleanup := func() {
		ofc.Free()
		ioCtx.Free()
	}
	return ofc, cleanup, nil
}

// buildOutputPlan adds output streams for the selected input streams to the muxer, deciding PER STREAM
// whether to COPY it or TRANSCODE it. Video is copied only when it's already H.264 AND the caller says the
// segment grid is keyframe-aligned (transcodeVideo=false); otherwise it is transcoded to H.264 — HEVC/etc.
// because MSE can't decode it, and H.264 on a UNIFORM grid because the boundaries aren't keyframes so a
// copy would seek back to an earlier keyframe and overlap the previous segment (the transcoder forces an
// IDR at the segment start instead). Audio is copied when AAC, else transcoded to AAC. streams[0] is the
// primary (seeked-on) stream; startTS/endTS (primary timebase) bound which decoded VIDEO frames the
// transcoder emits. Codec tags are cleared so the MP4 muxer assigns valid ones out of MKV (Review R5).
//
// Returns copyMap (src→out idx for copied streams), videoEncs and audioEncs (src→transcoder). The caller
// WriteHeader()s after, dispatches each packet to the matching encoder/copy, flushes every encoder, and
// must free() them all.
func buildOutputPlan(ifc, ofc *astiav.FormatContext, streams []int, startTS, endTS int64, transcodeVideo bool, fw *fragWriter) (map[int]int, map[int]*videoEncoder, map[int]*audioEncoder, error) {
	inStreams := ifc.Streams()
	copyMap := make(map[int]int)
	videoEncs := make(map[int]*videoEncoder)
	audioEncs := make(map[int]*audioEncoder)
	freeAll := func() {
		for _, v := range videoEncs {
			v.free()
		}
		for _, a := range audioEncs {
			a.free()
		}
	}
	for _, si := range streams {
		if si < 0 || si >= len(inStreams) {
			freeAll()
			return nil, nil, nil, fmt.Errorf("media: input stream %d out of range (have %d)", si, len(inStreams))
		}
		in := inStreams[si]
		mt := in.CodecParameters().MediaType()
		cid := in.CodecParameters().CodecID()
		switch {
		case mt == astiav.MediaTypeVideo && (transcodeVideo || cid != astiav.CodecIDH264):
			enc, err := newVideoEncoder(ifc, ofc, si, startTS, endTS, fw)
			if err != nil {
				freeAll()
				return nil, nil, nil, err
			}
			videoEncs[si] = enc
		case mt == astiav.MediaTypeAudio && cid != astiav.CodecIDAac:
			enc, err := newAudioEncoder(ifc, ofc, si, startTS, endTS, fw)
			if err != nil {
				freeAll()
				return nil, nil, nil, err
			}
			audioEncs[si] = enc
		default:
			out := ofc.NewStream(nil)
			if out == nil {
				freeAll()
				return nil, nil, nil, fmt.Errorf("media: new output stream")
			}
			if err := in.CodecParameters().Copy(out.CodecParameters()); err != nil {
				freeAll()
				return nil, nil, nil, fmt.Errorf("media: copy codec params (stream %d): %w", si, err)
			}
			out.CodecParameters().SetCodecTag(0)
			out.SetTimeBase(in.TimeBase())
			copyMap[si] = out.Index()
		}
	}
	return copyMap, videoEncs, audioEncs, nil
}
