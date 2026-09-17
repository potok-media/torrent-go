package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/asticode/go-astiav"
)

// noPTS mirrors AV_NOPTS_VALUE — packets without a presentation timestamp carry this sentinel.
const noPTS = math.MinInt64

// fragMovFlags configures the mov muxer for on-demand fMP4:
//   - empty_moov       → the init (ftyp+moov) is written at header time with no samples;
//   - frag_keyframe    → fragment at keyframes;
//   - default_base_moof→ each moof self-describes its base offset, so a media segment is independently
//     appendable after the init and its tfdt (kept absolute below) lets hls.js order segments (Review R6).
//
// initMovFlags configures the muxer for the INIT segment: empty_moov writes the full moov (codec config,
// no samples) at header time — the init IS the header.
const initMovFlags = "+empty_moov+frag_keyframe+default_base_moof"

// segMovFlags configures the muxer for a MEDIA segment. Every rendition segment is its own muxer
// instance continuing an EXTERNAL (whole-file) timeline, and movenc normally REBASES a fresh instance's
// track to start at 0 — every segment's tfdt would be 0 and hls.js would stack them all at the same
// buffer position (the buffer never fills at the playhead → the loader races/skips through the whole
// playlist). The combination that keeps tfdt ABSOLUTE (Review R6):
//   - delay_moov   → the moov isn't written until the first fragment flushes, so the first packet's
//     absolute dts is known BEFORE the timeline anchor is fixed (with empty_moov the moov is written at
//     header time and the anchor is locked to 0 before any packet arrives);
//   - frag_discont → movenc anchors the first fragment's tfdt at that incoming absolute dts instead of
//     rebasing to 0.
//
// The segment's own ftyp+moov prefix is stripped after WriteTrailer (the client uses the shared init).
const segMovFlags = "+delay_moov+frag_discont+frag_keyframe+default_base_moof"

func setFragOpts(dict *astiav.Dictionary, flags string) error {
	if err := dict.Set("movflags", flags, 0); err != nil {
		return fmt.Errorf("media: set movflags: %w", err)
	}
	return nil
}

// InitSegment produces the fMP4 initialization segment (ftyp+moov) shared by every media segment of this
// (source, stream-set) — i.e. the client's EXT-X-MAP. It is just the muxer header with no packets.
func InitSegment(ctx context.Context, src io.ReadSeeker, streams []int, transcodeVideo bool) ([]byte, error) {
	ifc, icleanup, err := openDemux(ctx, src)
	if err != nil {
		return nil, err
	}
	defer icleanup()

	mw := &memWriter{}
	ofc, ocleanup, err := openOutput(mw)
	if err != nil {
		return nil, err
	}
	defer ocleanup()

	// Same output-stream plan as Segment, so the init and the media segments describe IDENTICAL tracks —
	// crucially the H.264/AAC output streams for transcoded video/audio, not the source HEVC/AC3. No
	// packets are processed; opening the encoders is enough to emit their codec config into the moov (the
	// window bounds are irrelevant here since no frames are encoded).
	// No packets flow here, so the fragWriter is never written to — but the encoder constructors require one.
	_, videoEncs, audioEncs, err := buildOutputPlan(ifc, ofc, streams, 0, math.MaxInt64, transcodeVideo, newFragWriter(ofc))
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, v := range videoEncs {
			v.free()
		}
		for _, a := range audioEncs {
			a.free()
		}
	}()

	dict := astiav.NewDictionary()
	defer dict.Free()
	if err := setFragOpts(dict, initMovFlags); err != nil {
		return nil, err
	}
	if err := ofc.WriteHeader(dict); err != nil {
		return nil, fmt.Errorf("media: write init header: %w", err)
	}
	// The header IS the init segment (empty_moov emits the full moov here). We deliberately do NOT
	// WriteTrailer — there are no fragments to finalize and the trailer would append mfra/finalization.
	return mw.Bytes(), nil
}

// Segment demuxes [startSec, startSec+durSec) of src and copy-remuxes those packets into ONE moof-only
// fMP4 media segment, to be played after the shared InitSegment — no re-encode. Timestamps stay ABSOLUTE
// (never reset to zero) so each segment's tfdt aligns with the init timescale and hls.js sequences
// segments correctly. streams[0] is primary (seeked on; normally the video stream).
//
// This is the in-process replacement for one ffmpeg HLS segment: a function call fed by the RAM cache,
// bounded by ctx (the IOInterrupter in openDemux), with no subprocess, no loopback, no producer to reap.
func Segment(ctx context.Context, src io.ReadSeeker, startSec, durSec float64, streams []int, transcodeVideo bool) ([]byte, error) {
	if len(streams) == 0 {
		return nil, errors.New("media: no streams selected")
	}

	ifc, icleanup, err := openDemux(ctx, src)
	if err != nil {
		return nil, err
	}
	defer icleanup()

	mw := &memWriter{}
	ofc, ocleanup, err := openOutput(mw)
	if err != nil {
		return nil, err
	}
	defer ocleanup()

	inStreams := ifc.Streams()
	primary := streams[0]
	ptb := inStreams[primary].TimeBase()
	// ticks = seconds / (num/den) = seconds * den/num
	toTicks := func(sec float64) int64 { return int64(sec * float64(ptb.Den()) / float64(ptb.Num())) }
	startTS := toTicks(startSec)
	endTS := toTicks(startSec + durSec)

	fw := newFragWriter(ofc)
	defer fw.free()
	copyMap, videoEncs, audioEncs, err := buildOutputPlan(ifc, ofc, streams, startTS, endTS, transcodeVideo, fw)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, v := range videoEncs {
			v.free()
		}
		for _, a := range audioEncs {
			a.free()
		}
	}()

	dict := astiav.NewDictionary()
	defer dict.Free()
	if err := setFragOpts(dict, segMovFlags); err != nil {
		return nil, err
	}
	if err := ofc.WriteHeader(dict); err != nil {
		return nil, fmt.Errorf("media: write header: %w", err)
	}

	// Seek to at/just before the segment start (backward) — on the stream that actually HAS a container
	// index. MKV Cues index only the VIDEO track: seeking on an audio/subtitle stream (no index entries)
	// makes libav fall back to a LINEAR parse from the segment start to the target — seconds per call,
	// repeated for every produced segment. So position by the video stream whenever the input has one:
	// the demuxer lands on the cluster at/before startTS, which carries every track's packets, and the
	// per-stream [startTS,endTS) windows (video transcoder, audio encoder, audio copy) trim the pre-roll.
	seekIdx, seekTB := primary, ptb
	if inStreams[primary].CodecParameters().MediaType() != astiav.MediaTypeVideo {
		for _, s := range inStreams {
			// Skip cover-art stills (ATTACHED_PIC): a 1-frame image carries no usable container index to seek on.
			if s.CodecParameters().MediaType() == astiav.MediaTypeVideo && !s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic) {
				seekIdx, seekTB = s.Index(), s.TimeBase()
				break
			}
		}
	}
	seekTS := int64(startSec * float64(seekTB.Den()) / float64(seekTB.Num()))
	if err := ifc.SeekFrame(seekIdx, seekTS, astiav.SeekFlags(astiav.SeekFlagBackward)); err != nil {
		return nil, fmt.Errorf("media: seek: %w", err)
	}

	pkt := astiav.AllocPacket()
	defer pkt.Free()
	lastDTS := make(map[int]int64) // per output stream, for the copy-path dts sanitizer
	for {
		if err := ifc.ReadFrame(pkt); err != nil {
			if errors.Is(err, astiav.ErrEof) {
				break
			}
			pkt.Unref()
			return nil, fmt.Errorf("media: read frame: %w", err)
		}

		si := pkt.StreamIndex()
		// The PRIMARY stream ends the segment: once its packet pts crosses endTS the rest belongs to the
		// next segment. Checked before dispatch so it works whether the primary is copied or transcoded.
		if si == primary {
			if pkt.Pts() == noPTS {
				pkt.SetPts(pkt.Dts())
			}
			if pkt.Pts() != noPTS && pkt.Pts() >= endTS {
				pkt.Unref()
				break
			}
		}

		if enc, ok := videoEncs[si]; ok {
			if err := enc.feed(pkt); err != nil {
				pkt.Unref()
				return nil, err
			}
			pkt.Unref()
			continue
		}
		if enc, ok := audioEncs[si]; ok {
			if err := enc.feed(pkt); err != nil {
				pkt.Unref()
				return nil, err
			}
			pkt.Unref()
			continue
		}
		outIdx, keep := copyMap[si]
		if !keep {
			pkt.Unref()
			continue
		}
		// Audio copy: trim the pre-roll the backward seek reads before the grid boundary so segments abut.
		// (Video copy needs no trim — its grid is keyframe-aligned, so the boundary is exact.)
		if inStreams[si].CodecParameters().MediaType() == astiav.MediaTypeAudio {
			if p := pkt.Pts(); p != noPTS && (p < startTS || p >= endTS) {
				pkt.Unref()
				continue
			}
		}
		pkt.SetStreamIndex(outIdx)
		pkt.RescaleTs(inStreams[si].TimeBase(), ofc.Streams()[outIdx].TimeBase())
		sanitizeCopyDTS(pkt, outIdx, lastDTS)
		if err := fw.write(pkt); err != nil {
			pkt.Unref()
			return nil, err
		}
		pkt.Unref()
	}

	// Flush every transcoder (drain decoder/FIFO + encoder) before finalizing the fragment. The encoders write
	// through the same fragWriter, so their trailing packets are held too.
	for _, v := range videoEncs {
		if err := v.flush(); err != nil {
			return nil, err
		}
	}
	for _, a := range audioEncs {
		if err := a.flush(); err != nil {
			return nil, err
		}
	}
	// Flush every stream's last held packet with an exact duration — otherwise movenc ESTIMATES the last
	// sample's duration and the segment ends slightly off its grid boundary (the recurring HLS4 stall).
	if err := fw.finish(); err != nil {
		return nil, err
	}
	if err := ofc.WriteTrailer(); err != nil {
		return nil, fmt.Errorf("media: write trailer: %w", err)
	}

	// Strip the ftyp+moov init prefix — the client already has it from InitSegment — so this returns a
	// moof-only media segment.
	full := mw.Bytes()
	if off := firstBoxOffset(full, "moof"); off > 0 {
		return full[off:], nil
	}
	return full, nil
}

// sanitizeCopyDTS makes a copied packet's timestamps valid for the mp4 muxer, which requires a defined,
// strictly-increasing dts (and pts >= dts). The MKV demuxer leaves the FIRST packet after a seek with an
// unset dts (MKV stores only pts) — that's the per-segment "Timestamps are unset" case; a transient bad
// read at the download front can also regress the dts. We fill/repair dts and keep it monotonic per
// output stream. The first post-seek packet is the segment's keyframe, so dts=pts there is correct.
func sanitizeCopyDTS(pkt *astiav.Packet, outIdx int, last map[int]int64) {
	dts := pkt.Dts()
	if dts == noPTS {
		dts = pkt.Pts()
	}
	if l, ok := last[outIdx]; ok && dts != noPTS && dts <= l {
		dts = l + 1
	}
	if dts == noPTS {
		return // nothing to base it on — leave it to the muxer
	}
	last[outIdx] = dts
	pkt.SetDts(dts)
	if p := pkt.Pts(); p == noPTS || p < dts {
		pkt.SetPts(dts)
	}
}

// fragWriter delays each output stream's packets by ONE so it can stamp an explicit duration on every one.
// movenc derives a sample's duration from dts[i+1]-dts[i] for all samples EXCEPT the last of each fragment,
// which it ESTIMATES ("Estimating the duration of the last packet in a fragment") — and that estimate is
// inexact and DIFFERS between the independently-produced audio and video renditions, so their segment ends
// drift apart on the shared HLS timeline and hls.js stalls (bufferStalledError). Holding the previous packet
// lets us set its duration to the real dts gap; the final held packet gets the last such gap (the stream's
// natural frame period / audio frame size). We deliberately do NOT stretch the last packet to the grid
// boundary — audio frames are quantized (1024 samples) and wouldn't land on the video keyframe boundary, so
// stretching would open an audio gap. Natural durations make consecutive segments of EACH rendition abut
// exactly (seg[N].end == seg[N+1].start), which is what removes the drift.
type fragWriter struct {
	ofc     *astiav.FormatContext
	held    map[int]*astiav.Packet // per output stream: the packet awaiting its successor's dts
	lastDur map[int]int64          // per output stream: the last interior duration, reused for the final packet
}

func newFragWriter(ofc *astiav.FormatContext) *fragWriter {
	return &fragWriter{ofc: ofc, held: map[int]*astiav.Packet{}, lastDur: map[int]int64{}}
}

// write buffers pkt (holding a clone) and flushes the previously-held packet for its stream first, stamping it
// with the exact dts gap. The caller keeps ownership of pkt and may Unref it afterwards.
func (fw *fragWriter) write(pkt *astiav.Packet) error {
	si := pkt.StreamIndex()
	if prev := fw.held[si]; prev != nil {
		if d := pkt.Dts() - prev.Dts(); d > 0 {
			prev.SetDuration(d)
			fw.lastDur[si] = d
		}
		werr := fw.ofc.WriteInterleavedFrame(prev) // consumes prev's data ref
		prev.Free()
		if werr != nil {
			delete(fw.held, si)
			return fmt.Errorf("media: write frame: %w", werr)
		}
	}
	clone := pkt.Clone()
	if clone == nil {
		return errors.New("media: fragWriter: packet clone failed")
	}
	fw.held[si] = clone
	return nil
}

// finish flushes each stream's final held packet, stamping it with the last known interior duration so the
// segment's last sample carries a real duration (no movenc estimate). Call once before WriteTrailer.
func (fw *fragWriter) finish() error {
	for si, prev := range fw.held {
		dur := fw.lastDur[si]
		if dur <= 0 {
			dur = prev.Duration()
		}
		if dur <= 0 {
			dur = 1
		}
		prev.SetDuration(dur)
		werr := fw.ofc.WriteInterleavedFrame(prev)
		prev.Free()
		delete(fw.held, si)
		if werr != nil {
			return fmt.Errorf("media: write frame: %w", werr)
		}
	}
	return nil
}

// free releases any still-held packets (error paths); safe to defer.
func (fw *fragWriter) free() {
	for si, p := range fw.held {
		p.Free()
		delete(fw.held, si)
	}
}

// firstBoxOffset returns the byte offset of the first top-level MP4 box of the given 4-char type, or -1.
// MP4 boxes are [uint32 size][4-byte type]; size==1 means a 64-bit size follows the type, size==0 means
// "to EOF".
func firstBoxOffset(b []byte, want string) int64 {
	var off int64
	for off+8 <= int64(len(b)) {
		size := int64(binary.BigEndian.Uint32(b[off : off+4]))
		if string(b[off+4:off+8]) == want {
			return off
		}
		switch size {
		case 1:
			if off+16 > int64(len(b)) {
				return -1
			}
			size = int64(binary.BigEndian.Uint64(b[off+8 : off+16]))
		case 0:
			return -1
		}
		if size < 8 {
			return -1
		}
		off += size
	}
	return -1
}
