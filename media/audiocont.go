package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"

	"github.com/asticode/go-astiav"
)

// Continuous-AAC transcode for HLS4 (see plan vivid-moseying-sloth). A non-AAC audio track (AC3/EAC3/DTS/…) is
// decoded+re-encoded to ONE continuous AAC stream ONCE, cached as encoded frames; an HLS audio segment is then a
// pure COPY-slice of those frames. This is the whole fix: the AAC-copy path tiles perfectly because it slices one
// continuous stream by pts, whereas per-segment re-encoding gives every segment its own AAC frame phase + priming,
// so consecutive segments overlap (~22ms) and hls.js throws bufferAppendError. One continuous encode ⇒ one frame
// grid, one priming ⇒ segments abut exactly like copy.

// aacEncoderDelay is the native ffmpeg AAC encoder's fixed 1024-sample encoder delay (one frame of MDCT
// lookahead). ffmpeg sets it via avctx->initial_padding, which the normal encoder→muxer pipeline turns into an
// edit list — but we re-inject encoded packets into a SEPARATE muxer (and go-astiav exposes neither
// initial_padding nor, for this encoder, SKIP_SAMPLES side data), so we fold this known delay out of the clock
// ourselves. Used only when the encoder doesn't report a priming via SKIP_SAMPLES.
const aacEncoderDelay = 1024

// aacFrame is one encoded AAC access unit on the CONTINUOUS absolute sample clock (timebase 1/sampleRate). The
// encoder priming is folded out of pts (see transcode), so pts is the frame's TRUE wall-clock audio position.
type aacFrame struct {
	pts  int64
	dur  int64
	data []byte
}

// ContinuousAAC holds the whole track's AAC frames plus the frozen codec config for the shared init. All fields
// are guarded by mu. The producing goroutine (transcode) is the only writer of frames/priming/producedTo; readers
// (SegmentFromAAC/InitFromAAC/Status) snapshot under mu.
type ContinuousAAC struct {
	mu         sync.Mutex
	startSeg   int
	startSec   float64
	frames     []aacFrame              // append-only, pts-monotonic
	sampleRate int                     // AAC output rate (== source rate)
	codecPar   *astiav.CodecParameters // frozen AAC config (ASC extradata) for the init; nil until the encoder opens
	priming    int64                   // encoder delay (SkipStart of packet 0), folded out of every pts; set once
	producedTo int64                   // highest true audio pts appended (samples); the wait loop polls this
	complete   bool                    // clean EOF: the whole track is transcoded
	err        error                   // terminal transcode error (set immediately, not only at EOF)
	cancel     context.CancelFunc      // stops Run (set by the owner)
}

// NewContinuousAAC returns an empty cache. The owner starts Run in a goroutine and stores cancel via SetCancel.
func NewContinuousAAC(startSeg int, startSec float64) *ContinuousAAC {
	return &ContinuousAAC{
		startSeg: startSeg,
		startSec: startSec,
	}
}

func (c *ContinuousAAC) StartSec() float64 {
	return c.startSec
}

func (c *ContinuousAAC) StartSeg() int {
	return c.startSeg
}

func (c *ContinuousAAC) SetCancel(cf context.CancelFunc) { c.cancel = cf }

// Cancel stops the transcoder goroutine (idempotent).
func (c *ContinuousAAC) Cancel() {
	if c.cancel != nil {
		c.cancel()
	}
}

// Fail records a terminal error before Run starts (e.g. the reader couldn't be opened), so waiters fail fast.
func (c *ContinuousAAC) Fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

// Status is the wait-loop's view: how far the transcode has reached, whether it finished, and any terminal error.
func (c *ContinuousAAC) Status() (producedTo int64, complete bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.producedTo, c.complete, c.err
}

// Ready reports whether the codec config is known (⇒ InitFromAAC can be produced).
func (c *ContinuousAAC) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.codecPar != nil
}

// SampleRate is the AAC output sample rate (0 until the encoder opens).
func (c *ContinuousAAC) SampleRate() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sampleRate
}

// Free releases the cached frames + codec config. Call after Cancel + the goroutine has exited.
func (c *ContinuousAAC) Free() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.codecPar != nil {
		c.codecPar.Free()
		c.codecPar = nil
	}
	c.frames = nil
}

// Run transcodes the whole track (blocking; the owner runs it in a goroutine). On clean EOF it sets complete;
// on any error it sets err. src must be positioned at 0 and cover the whole file (a ClassPlayback torrent reader
// blocks on non-resident pieces, so the transcode naturally follows the download).
func (c *ContinuousAAC) Run(ctx context.Context, src io.ReadSeeker, srcIdx int) {
	err := c.transcode(ctx, src, srcIdx)
	c.mu.Lock()
	if err != nil {
		if c.err == nil {
			c.err = err
		}
	} else {
		c.complete = true
	}
	c.mu.Unlock()
}

func (c *ContinuousAAC) transcode(ctx context.Context, src io.ReadSeeker, srcIdx int) error {
	ifc, cleanup, err := openDemux(ctx, src)
	if err != nil {
		return err
	}
	defer cleanup()

	inStreams := ifc.Streams()
	if srcIdx < 0 || srcIdx >= len(inStreams) {
		return fmt.Errorf("media: audiocont: stream %d out of range (have %d)", srcIdx, len(inStreams))
	}
	in := inStreams[srcIdx]

	if c.startSec > 0 {
		seekIdx, seekTB := srcIdx, in.TimeBase()
		for _, s := range inStreams {
			if s.CodecParameters().MediaType() == astiav.MediaTypeVideo && !s.DispositionFlags().Has(astiav.DispositionFlagAttachedPic) {
				seekIdx, seekTB = s.Index(), s.TimeBase()
				break
			}
		}
		ts := int64(c.startSec * float64(seekTB.Den()) / float64(seekTB.Num()))
		if err := ifc.SeekFrame(seekIdx, ts, astiav.NewSeekFlags(astiav.SeekFlagBackward)); err != nil {
			slog.Warn("audiocont: seek failed", "ts", ts, "err", err)
		}
	}

	decCodec := astiav.FindDecoder(in.CodecParameters().CodecID())
	if decCodec == nil {
		return fmt.Errorf("media: audiocont: no decoder for %s", in.CodecParameters().CodecID().String())
	}
	dec := astiav.AllocCodecContext(decCodec)
	if dec == nil {
		return errors.New("media: audiocont: alloc decoder")
	}
	defer dec.Free()
	if err := in.CodecParameters().ToCodecContext(dec); err != nil {
		return fmt.Errorf("media: audiocont: decoder params: %w", err)
	}
	if err := dec.Open(decCodec, nil); err != nil {
		return fmt.Errorf("media: audiocont: open decoder: %w", err)
	}

	encCodec := astiav.FindEncoder(astiav.CodecIDAac)
	if encCodec == nil {
		return errors.New("media: audiocont: no AAC encoder")
	}
	enc := astiav.AllocCodecContext(encCodec)
	if enc == nil {
		return errors.New("media: audiocont: alloc AAC encoder")
	}
	defer enc.Free()
	enc.SetSampleRate(dec.SampleRate())
	// Downmix to stereo — matches the per-segment audioEncoder (multichannel AAC needs a PCE some MSE decoders
	// choke on); the resampler downmixes automatically.
	enc.SetChannelLayout(astiav.ChannelLayoutStereo)
	if sfs := encCodec.SampleFormats(); len(sfs) > 0 {
		enc.SetSampleFormat(sfs[0]) // AAC → fltp
	} else {
		enc.SetSampleFormat(dec.SampleFormat())
	}
	enc.SetTimeBase(astiav.NewRational(1, dec.SampleRate()))
	// MP4 needs the AAC config in extradata (AudioSpecificConfig), not inline in packets — force GLOBAL_HEADER so
	// FromCodecContext below captures the ASC into codecPar (verified: extradata is copied only when this is set).
	enc.SetFlags(enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	if err := enc.Open(encCodec, nil); err != nil {
		return fmt.Errorf("media: audiocont: open AAC encoder: %w", err)
	}

	// Freeze the AAC config for the init segment (available near-instantly, before any frame is encoded).
	cp := astiav.AllocCodecParameters()
	if err := cp.FromCodecContext(enc); err != nil {
		cp.Free()
		return fmt.Errorf("media: audiocont: codecpar from encoder: %w", err)
	}
	c.mu.Lock()
	c.sampleRate = enc.SampleRate()
	c.codecPar = cp
	c.mu.Unlock()

	swr := astiav.AllocSoftwareResampleContext()
	defer swr.Free()
	fifo := astiav.AllocAudioFifo(enc.SampleFormat(), enc.ChannelLayout().Channels(), 1)
	defer fifo.Free()
	decFrame := astiav.AllocFrame()
	defer decFrame.Free()
	resampled := astiav.AllocFrame()
	defer resampled.Free()
	outFrame := astiav.AllocFrame()
	defer outFrame.Free()
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	encPkt := astiav.AllocPacket()
	defer encPkt.Free()

	inTB := in.TimeBase()
	frameSize := enc.FrameSize()
	if frameSize <= 0 {
		frameSize = 1024
	}

	var nextPts int64
	started := false
	primingRead := false

	// drainEncoder pulls every AAC packet the encoder emits and appends it as a frame with priming folded out.
	drainEncoder := func() error {
		for {
			rerr := enc.ReceivePacket(encPkt)
			if errors.Is(rerr, astiav.ErrEof) || errors.Is(rerr, astiav.ErrEagain) {
				return nil
			}
			if rerr != nil {
				return fmt.Errorf("media: audiocont: receive packet: %w", rerr)
			}
			c.mu.Lock()
			if !primingRead {
				// Priming (encoder delay) is a STREAM property counted ONCE here so every frame's pts becomes the
				// TRUE wall-clock position (real audio lands at what was packet `priming`). Prefer the encoder's
				// SKIP_SAMPLES side data; the native ffmpeg AAC encoder doesn't emit it, so fall back to its known
				// fixed delay (else the whole track would sit ~21ms late vs video).
				if ss, ok := encPkt.SideData().SkipSamples().Get(); ok && ss.SkipStart > 0 {
					c.priming = int64(ss.SkipStart)
				} else {
					c.priming = aacEncoderDelay
				}
				primingRead = true
			}
			data := make([]byte, len(encPkt.Data()))
			copy(data, encPkt.Data())
			truePts := encPkt.Pts() - c.priming
			c.frames = append(c.frames, aacFrame{pts: truePts, dur: int64(frameSize), data: data})
			c.producedTo = truePts + int64(frameSize)
			c.mu.Unlock()
			encPkt.Unref()
		}
	}

	// drainFifo re-frames buffered PCM into fixed-size AAC frames and encodes them.
	drainFifo := func(flush bool) error {
		for fifo.Size() >= frameSize || (flush && fifo.Size() > 0) {
			n := frameSize
			if fifo.Size() < n {
				n = fifo.Size()
			}
			outFrame.Unref()
			outFrame.SetSampleFormat(enc.SampleFormat())
			outFrame.SetChannelLayout(enc.ChannelLayout())
			outFrame.SetSampleRate(enc.SampleRate())
			outFrame.SetNbSamples(n)
			if err := outFrame.AllocBuffer(0); err != nil {
				return fmt.Errorf("media: audiocont: alloc out buffer: %w", err)
			}
			if _, err := fifo.Read(outFrame); err != nil {
				return fmt.Errorf("media: audiocont: fifo read: %w", err)
			}
			outFrame.SetPts(nextPts)
			nextPts += int64(n)
			if err := enc.SendFrame(outFrame); err != nil {
				return fmt.Errorf("media: audiocont: send frame: %w", err)
			}
			if err := drainEncoder(); err != nil {
				return err
			}
		}
		return nil
	}

	// pushFrame resamples one decoded frame into the FIFO and encodes any full frames now available.
	pushFrame := func() error {
		if !started {
			// Seed the continuous sample clock from the FIRST decoded frame's absolute source pts (variant A:
			// frame pts already carry the audio stream's start offset; segment selection uses the video base).
			if decFrame.Pts() != noPTS {
				nextPts = int64(float64(decFrame.Pts()) * float64(inTB.Num()) / float64(inTB.Den()) * float64(enc.SampleRate()))
			}
			started = true
		}
		resampled.Unref()
		resampled.SetSampleFormat(enc.SampleFormat())
		resampled.SetChannelLayout(enc.ChannelLayout())
		resampled.SetSampleRate(enc.SampleRate())
		resampled.SetNbSamples(decFrame.NbSamples())
		if err := resampled.AllocBuffer(0); err != nil {
			return fmt.Errorf("media: audiocont: alloc resample buffer: %w", err)
		}
		if err := swr.ConvertFrame(decFrame, resampled); err != nil {
			return fmt.Errorf("media: audiocont: resample: %w", err)
		}
		if _, err := fifo.Write(resampled); err != nil {
			return fmt.Errorf("media: audiocont: fifo write: %w", err)
		}
		return drainFifo(false)
	}

	drainDecoder := func() error {
		for {
			derr := dec.ReceiveFrame(decFrame)
			if errors.Is(derr, astiav.ErrEof) || errors.Is(derr, astiav.ErrEagain) {
				return nil
			}
			if derr != nil {
				return fmt.Errorf("media: audiocont: receive frame: %w", derr)
			}
			err := pushFrame()
			decFrame.Unref()
			if err != nil {
				return err
			}
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rerr := ifc.ReadFrame(pkt)
		if rerr != nil {
			if errors.Is(rerr, astiav.ErrEof) {
				break
			}
			pkt.Unref()
			return fmt.Errorf("media: audiocont: read frame: %w", rerr)
		}
		if pkt.StreamIndex() != srcIdx {
			pkt.Unref()
			continue
		}
		if err := dec.SendPacket(pkt); err != nil {
			pkt.Unref()
			return fmt.Errorf("media: audiocont: send packet: %w", err)
		}
		pkt.Unref()
		if err := drainDecoder(); err != nil {
			return err
		}
	}

	// Flush: decoder → FIFO remainder → encoder.
	if err := dec.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return fmt.Errorf("media: audiocont: flush decoder: %w", err)
	}
	if err := drainDecoder(); err != nil {
		return err
	}
	if err := drainFifo(true); err != nil {
		return err
	}
	if err := enc.SendFrame(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return fmt.Errorf("media: audiocont: flush encoder: %w", err)
	}
	return drainEncoder()
}

// SegmentFromAAC produces a moof-only fMP4 audio media segment covering the sample window [lo, hi) (in the
// continuous AAC clock) by COPY-slicing cached frames — no decode/seek/demux. Snapshots the frame range under mu
// (frame data is immutable after append), then builds the segment OUTSIDE mu so a concurrent append/eviction can't
// race the reader or block on the muxer's allocations.
func SegmentFromAAC(cont *ContinuousAAC, lo, hi int64) ([]byte, error) {
	cont.mu.Lock()
	if cont.codecPar == nil {
		cont.mu.Unlock()
		return nil, errors.New("media: audiocont: codec config not ready")
	}
	cp := astiav.AllocCodecParameters()
	if err := cont.codecPar.Copy(cp); err != nil {
		cont.mu.Unlock()
		cp.Free()
		return nil, fmt.Errorf("media: audiocont: copy codecpar: %w", err)
	}
	sampleRate := cont.sampleRate
	frames := cont.frames
	i := sort.Search(len(frames), func(k int) bool { return frames[k].pts >= lo })
	var snap []aacFrame
	for ; i < len(frames) && frames[i].pts < hi; i++ {
		f := frames[i]
		d := make([]byte, len(f.data))
		copy(d, f.data)
		snap = append(snap, aacFrame{pts: f.pts, dur: f.dur, data: d})
	}
	cont.mu.Unlock()
	defer cp.Free()

	if len(snap) == 0 {
		return nil, fmt.Errorf("media: audiocont: no frames in [%d,%d)", lo, hi)
	}

	mw := &memWriter{}
	ofc, ocleanup, err := openOutput(mw)
	if err != nil {
		return nil, err
	}
	defer ocleanup()
	out := ofc.NewStream(nil)
	if out == nil {
		return nil, errors.New("media: audiocont: new output stream")
	}
	if err := cp.Copy(out.CodecParameters()); err != nil {
		return nil, fmt.Errorf("media: audiocont: stream codecpar: %w", err)
	}
	out.CodecParameters().SetCodecTag(0)
	out.SetTimeBase(astiav.NewRational(1, sampleRate))

	dict := astiav.NewDictionary()
	defer dict.Free()
	if err := setFragOpts(dict, segMovFlags); err != nil {
		return nil, err
	}
	if err := ofc.WriteHeader(dict); err != nil {
		return nil, fmt.Errorf("media: audiocont: write header: %w", err)
	}

	fw := newFragWriter(ofc)
	defer fw.free()
	pkt := astiav.AllocPacket()
	defer pkt.Free()
	for _, f := range snap {
		pkt.Unref()
		if err := pkt.FromData(f.data); err != nil {
			return nil, fmt.Errorf("media: audiocont: packet from data: %w", err)
		}
		pkt.SetStreamIndex(out.Index())
		pkt.SetPts(f.pts)
		pkt.SetDts(f.pts) // AAC has no B-frames → dts == pts
		pkt.SetDuration(f.dur)
		if err := fw.write(pkt); err != nil {
			return nil, err
		}
	}
	if err := fw.finish(); err != nil {
		return nil, err
	}
	if err := ofc.WriteTrailer(); err != nil {
		return nil, fmt.Errorf("media: audiocont: write trailer: %w", err)
	}

	full := mw.Bytes()
	if off := firstBoxOffset(full, "moof"); off > 0 {
		return full[off:], nil
	}
	return full, nil
}

// InitFromAAC produces the shared fMP4 init (ftyp+moov) for the continuous AAC track from its frozen codec config.
// No edit list — go-astiav can't write one; priming is folded into the frame clock instead (see plan §5).
func InitFromAAC(cont *ContinuousAAC) ([]byte, error) {
	cont.mu.Lock()
	if cont.codecPar == nil {
		cont.mu.Unlock()
		return nil, errors.New("media: audiocont: codec config not ready")
	}
	cp := astiav.AllocCodecParameters()
	if err := cont.codecPar.Copy(cp); err != nil {
		cont.mu.Unlock()
		cp.Free()
		return nil, fmt.Errorf("media: audiocont: copy codecpar: %w", err)
	}
	sampleRate := cont.sampleRate
	cont.mu.Unlock()
	defer cp.Free()

	mw := &memWriter{}
	ofc, ocleanup, err := openOutput(mw)
	if err != nil {
		return nil, err
	}
	defer ocleanup()
	out := ofc.NewStream(nil)
	if out == nil {
		return nil, errors.New("media: audiocont: new output stream")
	}
	if err := cp.Copy(out.CodecParameters()); err != nil {
		return nil, fmt.Errorf("media: audiocont: stream codecpar: %w", err)
	}
	out.CodecParameters().SetCodecTag(0)
	out.SetTimeBase(astiav.NewRational(1, sampleRate))

	dict := astiav.NewDictionary()
	defer dict.Free()
	if err := setFragOpts(dict, initMovFlags); err != nil {
		return nil, err
	}
	if err := ofc.WriteHeader(dict); err != nil {
		return nil, fmt.Errorf("media: audiocont: write init header: %w", err)
	}
	return mw.Bytes(), nil
}
