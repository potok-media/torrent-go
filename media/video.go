package media

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/asticode/go-astiav"
)

// videoEncoder transcodes one incompatible source video stream (HEVC/VP9/AV1/10-bit/…) to browser-playable
// H.264, in-process: decode → convert to 8-bit yuv420p when needed → re-encode. It only encodes decoded
// frames whose pts falls in [startTS, endTS): frames before startTS are pre-roll (needed to decode the
// segment's first real frame when the segment boundary isn't a keyframe — the transcode/uniform grid) and
// frames at/after endTS belong to the next segment. A fresh encoder per segment ⇒ the first emitted frame
// is an IDR keyframe, so each segment is independently decodable; timestamps stay absolute (R6).
type videoEncoder struct {
	ofc    *astiav.FormatContext
	outIdx int
	w      *fragWriter // shared one-packet-delay writer: stamps exact per-packet durations (gapless segments)

	dec         *astiav.CodecContext
	enc         *astiav.CodecContext
	hwDevCtx    *astiav.HardwareDeviceContext
	sws         *astiav.SoftwareScaleContext
	decFrm      *astiav.Frame
	transferFrm *astiav.Frame
	scaled      *astiav.Frame
	pkt         *astiav.Packet
	bsf         *astiav.BitStreamFilterContext // mpeg4_unpack_bframes for DivX/Xvid packed bitstream; nil otherwise
	bsfPkt      *astiav.Packet                 // scratch for BSF output packets; nil when bsf is nil

	startTS    int64
	endTS      int64
	lastPTS    int64 // last decoded-frame pts fed to the encoder — monotonic guard (noPTS = none yet)
	lastDTS    int64 // last output-packet dts written to the muxer — monotonic guard (noPTS = none yet)
	inTimeBase astiav.Rational
}

// mpeg4UnpackBSF returns an initialized mpeg4_unpack_bframes bitstream filter for an MPEG-4 Part 2 (DivX/Xvid)
// input stream, or (nil,nil) for any other codec / if the filter is unavailable. DivX/Xvid PACKED BITSTREAM
// stores a P-frame and the following B-frame in ONE packet; decoded raw that yields frames with non-monotonic
// / NOPTS timestamps, which make libx264 emit a dts the mp4 muxer rejects (hard 500). The filter splits them
// into one clean packet per frame. Best-effort: any setup error → no filter (falls back to raw decode).
func mpeg4UnpackBSF(in *astiav.Stream) (*astiav.BitStreamFilterContext, *astiav.Packet) {
	if in.CodecParameters().CodecID() != astiav.CodecIDMpeg4 {
		return nil, nil
	}
	bsf := astiav.FindBitStreamFilterByName("mpeg4_unpack_bframes")
	if bsf == nil {
		return nil, nil
	}
	bsfc, err := astiav.AllocBitStreamFilterContext(bsf)
	if err != nil {
		return nil, nil
	}
	if err := in.CodecParameters().Copy(bsfc.InputCodecParameters()); err != nil {
		bsfc.Free()
		return nil, nil
	}
	bsfc.SetInputTimeBase(in.TimeBase())
	if err := bsfc.Initialize(); err != nil {
		bsfc.Free()
		return nil, nil
	}
	return bsfc, astiav.AllocPacket()
}

// newVideoEncoder wires the decode→(scale)→H.264-encode pipeline for input stream srcIdx and adds the
// H.264 output stream to ofc (before WriteHeader). Caller must free() it.
func newVideoEncoder(ifc, ofc *astiav.FormatContext, srcIdx int, startTS, endTS int64, fw *fragWriter) (*videoEncoder, error) {
	in := ifc.Streams()[srcIdx]

	decCodec := astiav.FindDecoder(in.CodecParameters().CodecID())
	if decCodec == nil {
		return nil, fmt.Errorf("media: no video decoder for %s", in.CodecParameters().CodecID().String())
	}
	dec := astiav.AllocCodecContext(decCodec)
	if dec == nil {
		return nil, fmt.Errorf("media: alloc video decoder")
	}
	if err := in.CodecParameters().ToCodecContext(dec); err != nil {
		dec.Free()
		return nil, fmt.Errorf("media: video decoder params: %w", err)
	}

	var hwDevCtx *astiav.HardwareDeviceContext
	var hwPixFmt astiav.PixelFormat = astiav.PixelFormatNone

	if ActiveGPU != nil {
		supportsHW := false
		for _, cfg := range decCodec.HardwareConfigs() {
			if cfg.HardwareDeviceType() == ActiveGPU.HwType && cfg.MethodFlags().Has(astiav.CodecHardwareConfigMethodFlagHwDeviceCtx) {
				supportsHW = true
				hwPixFmt = cfg.PixelFormat()
				break
			}
		}

		if supportsHW {
			device := ""
			if ActiveGPU.HwTypeName == "vaapi" {
				device = findRenderNode()
			}
			var err error
			hwDevCtx, err = astiav.CreateHardwareDeviceContext(ActiveGPU.HwType, device, nil, 0)
			if err != nil {
				slog.Warn("Failed to create hardware device context for decoding, falling back to software decoder", "err", err)
				hwDevCtx = nil
			} else if hwPixFmt != astiav.PixelFormatNone {
				dec.SetHardwareDeviceContext(hwDevCtx)
				dec.SetPixelFormatCallback(func(pfs []astiav.PixelFormat) astiav.PixelFormat {
					for _, pf := range pfs {
						if pf == hwPixFmt {
							return pf
						}
					}
					return astiav.PixelFormatNone
				})
			} else {
				hwDevCtx.Free()
				hwDevCtx = nil
			}
		}
	}

	if err := dec.Open(decCodec, nil); err != nil {
		dec.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: open video decoder: %w", err)
	}

	var encCodec *astiav.Codec
	if ActiveGPU != nil && hwDevCtx != nil {
		encCodec = astiav.FindEncoderByName(ActiveGPU.EncoderName)
	}
	if encCodec == nil {
		encCodec = astiav.FindEncoder(astiav.CodecIDH264)
	}
	if encCodec == nil {
		dec.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: no H.264 encoder available")
	}

	enc := astiav.AllocCodecContext(encCodec)
	if enc == nil {
		dec.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: alloc H.264 encoder")
	}
	enc.SetWidth(dec.Width())
	enc.SetHeight(dec.Height())

	// Decide pixel format for encoder
	var encPixFmt astiav.PixelFormat = astiav.PixelFormatYuv420P
	hasNv12 := false
	hasVaapi := false
	for _, pf := range encCodec.PixelFormats() {
		if pf == astiav.PixelFormatNv12 {
			hasNv12 = true
		}
		if pf == astiav.PixelFormatVaapi {
			hasVaapi = true
		}
	}
	if hasVaapi && encCodec.Name() == "h264_vaapi" {
		encPixFmt = astiav.PixelFormatVaapi
	} else if hasNv12 && (encCodec.Name() == "h264_videotoolbox" || encCodec.Name() == "h264_nvenc") {
		encPixFmt = astiav.PixelFormatNv12
	}
	enc.SetPixelFormat(encPixFmt)

	enc.SetSampleAspectRatio(dec.SampleAspectRatio())

	// Proper FPS and Timebase mapping to prevent level/MB-rate issues
	fps := in.AvgFrameRate()
	if fps.Num() > 0 && fps.Den() > 0 {
		enc.SetFramerate(fps)
		enc.SetTimeBase(fps.Invert())
	} else {
		enc.SetTimeBase(in.TimeBase())
	}

	if hwDevCtx != nil {
		enc.SetHardwareDeviceContext(hwDevCtx)
		if encPixFmt == astiav.PixelFormatVaapi {
			hwFramesCtx := astiav.AllocHardwareFramesContext(hwDevCtx)
			if hwFramesCtx == nil {
				dec.Free()
				enc.Free()
				hwDevCtx.Free()
				return nil, fmt.Errorf("media: alloc hardware frames context")
			}
			hwFramesCtx.SetHardwarePixelFormat(astiav.PixelFormatVaapi)
			hwFramesCtx.SetSoftwarePixelFormat(astiav.PixelFormatNv12)
			hwFramesCtx.SetWidth(dec.Width())
			hwFramesCtx.SetHeight(dec.Height())
			hwFramesCtx.SetInitialPoolSize(20)
			if err := hwFramesCtx.Initialize(); err != nil {
				hwFramesCtx.Free()
				dec.Free()
				enc.Free()
				hwDevCtx.Free()
				return nil, fmt.Errorf("media: initialize hardware frames context: %w", err)
			}
			enc.SetHardwareFramesContext(hwFramesCtx)
			hwFramesCtx.Free()
		}
	}

	if ofc.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		enc.SetFlags(enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	opts := astiav.NewDictionary()
	defer opts.Free()
	if encCodec.Name() == "h264_nvenc" {
		opts.Set("preset", "p4", 0)
		opts.Set("rc", "vbr", 0)
		opts.Set("cq", "24", 0)
	} else if encCodec.Name() == "h264_vaapi" {
		opts.Set("rc_mode", "CQP", 0)
		opts.Set("qp", "24", 0)
	} else if encCodec.Name() == "h264_videotoolbox" {
		opts.Set("realtime", "1", 0)
	} else {
		opts.Set("preset", "veryfast", 0)
		opts.Set("crf", "23", 0)
	}

	if err := enc.Open(encCodec, opts); err != nil {
		dec.Free()
		enc.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: open H.264 encoder: %w", err)
	}

	out := ofc.NewStream(nil)
	if out == nil {
		dec.Free()
		enc.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: new video output stream")
	}
	if err := out.CodecParameters().FromCodecContext(enc); err != nil {
		dec.Free()
		enc.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: video encoder params -> stream: %w", err)
	}
	out.SetTimeBase(enc.TimeBase())

	bsf, bsfPkt := mpeg4UnpackBSF(in)
	var transferFrm *astiav.Frame
	if hwDevCtx != nil && encPixFmt != astiav.PixelFormatVaapi {
		transferFrm = astiav.AllocFrame()
	}

	return &videoEncoder{
		ofc:         ofc,
		outIdx:      out.Index(),
		w:           fw,
		dec:         dec,
		enc:         enc,
		hwDevCtx:    hwDevCtx,
		inTimeBase:  in.TimeBase(),
		decFrm:      astiav.AllocFrame(),
		transferFrm: transferFrm,
		scaled:      astiav.AllocFrame(),
		pkt:         astiav.AllocPacket(),
		bsf:         bsf,
		bsfPkt:      bsfPkt,
		startTS:     startTS,
		endTS:       endTS,
		lastPTS:     noPTS,
		lastDTS:     noPTS,
	}, nil
}

func (v *videoEncoder) free() {
	v.pkt.Free()
	if v.bsfPkt != nil {
		v.bsfPkt.Free()
	}
	if v.bsf != nil {
		v.bsf.Free()
	}
	v.scaled.Free()
	if v.transferFrm != nil {
		v.transferFrm.Free()
	}
	v.decFrm.Free()
	if v.sws != nil {
		v.sws.Free()
	}
	v.enc.Free()
	v.dec.Free()
	if v.hwDevCtx != nil {
		v.hwDevCtx.Free()
	}
}

// feed runs one source video packet through the optional BSF, then decodes + encodes the in-window frames.
func (v *videoEncoder) feed(pkt *astiav.Packet) error {
	if v.bsf == nil {
		return v.decodeAndDrain(pkt)
	}
	if err := v.bsf.SendPacket(pkt); err != nil {
		return fmt.Errorf("media: video bsf send: %w", err)
	}
	return v.drainBSF()
}

// drainBSF pulls every packet the BSF emits for the last SendPacket and decodes each one.
func (v *videoEncoder) drainBSF() error {
	for {
		err := v.bsf.ReceivePacket(v.bsfPkt)
		if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: video bsf receive: %w", err)
		}
		derr := v.decodeAndDrain(v.bsfPkt)
		v.bsfPkt.Unref()
		if derr != nil {
			return derr
		}
	}
}

func (v *videoEncoder) decodeAndDrain(pkt *astiav.Packet) error {
	if err := v.dec.SendPacket(pkt); err != nil {
		return fmt.Errorf("media: video send packet: %w", err)
	}
	return v.drainDecoder()
}

func (v *videoEncoder) drainDecoder() error {
	for {
		err := v.dec.ReceiveFrame(v.decFrm)
		if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: video receive frame: %w", err)
		}
		// Only encode frames that sit on a strictly-increasing timeline inside the window. A frame with no
		// pts, or a pts that regresses/repeats (residual packed-bitstream / VFR weirdness the BSF didn't
		// resolve), can't be placed and would make libx264 emit a non-monotonic dts the muxer rejects.
		pts := v.decFrm.Pts()
		if pts == noPTS || pts < v.startTS || pts >= v.endTS {
			v.decFrm.Unref()
			continue
		}
		if v.lastPTS != noPTS && pts <= v.lastPTS {
			v.decFrm.Unref()
			continue
		}
		v.lastPTS = pts
		v.decFrm.SetPts(pts) // hand the encoder a clean, monotonic pts
		if err := v.encodeDecoded(); err != nil {
			v.decFrm.Unref()
			return err
		}
		v.decFrm.Unref()
	}
}

// encodeDecoded converts the current decoded frame to 8-bit yuv420p if needed, then encodes it.
func (v *videoEncoder) encodeDecoded() error {
	frame := v.decFrm
	if v.hwDevCtx != nil && v.transferFrm != nil {
		v.transferFrm.Unref()
		if err := v.decFrm.TransferHardwareData(v.transferFrm); err != nil {
			return fmt.Errorf("media: transfer hardware data: %w", err)
		}
		v.transferFrm.SetPts(v.decFrm.Pts())
		frame = v.transferFrm
	}

	targetFmt := v.enc.PixelFormat()
	if frame.PixelFormat() != targetFmt {
		if v.sws == nil {
			var err error
			v.sws, err = astiav.CreateSoftwareScaleContext(
				frame.Width(), frame.Height(), frame.PixelFormat(),
				frame.Width(), frame.Height(), targetFmt,
				astiav.SoftwareScaleContextFlags(astiav.SoftwareScaleContextFlagBilinear),
			)
			if err != nil {
				return fmt.Errorf("media: create sws: %w", err)
			}
		}
		v.scaled.Unref()
		v.scaled.SetWidth(frame.Width())
		v.scaled.SetHeight(frame.Height())
		v.scaled.SetPixelFormat(targetFmt)
		if err := v.sws.ScaleFrame(frame, v.scaled); err != nil {
			return fmt.Errorf("media: scale: %w", err)
		}
		v.scaled.SetPts(frame.Pts())
		frame = v.scaled
	}

	// Rescale PTS to encoder timebase to prevent timebase mismatch (R6)
	pts := astiav.RescaleQ(frame.Pts(), v.inTimeBase, v.enc.TimeBase())
	frame.SetPts(pts)

	// Clear the source picture type: a uniform-grid segment starts MID-GOP, so the decoded first frame is
	// a B/P frame. Passing that type to libx264 conflicts with "frame 0 must be an IDR" ("specified frame
	// type … not compatible with keyframe interval") and skews frame typing. NONE lets the encoder decide.
	frame.SetPictureType(astiav.PictureTypeNone)
	return v.encode(frame)
}

// encode sends one frame (nil = flush) to the H.264 encoder and writes every packet it emits.
func (v *videoEncoder) encode(f *astiav.Frame) error {
	if err := v.enc.SendFrame(f); err != nil {
		return fmt.Errorf("media: video send frame: %w", err)
	}
	for {
		err := v.enc.ReceivePacket(v.pkt)
		if errors.Is(err, astiav.ErrEof) || errors.Is(err, astiav.ErrEagain) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: video receive packet: %w", err)
		}
		v.pkt.SetStreamIndex(v.outIdx)
		v.pkt.RescaleTs(v.enc.TimeBase(), v.ofc.Streams()[v.outIdx].TimeBase())
		// Belt-and-suspenders: keep output dts strictly increasing so the mp4 muxer can never hard-fail
		// ("non monotonically increasing dts"), whatever the encoder emits (mirrors sanitizeCopyDTS).
		if dts := v.pkt.Dts(); dts != noPTS {
			if v.lastDTS != noPTS && dts <= v.lastDTS {
				dts = v.lastDTS + 1
				v.pkt.SetDts(dts)
				if p := v.pkt.Pts(); p == noPTS || p < dts {
					v.pkt.SetPts(dts)
				}
			}
			v.lastDTS = dts
		}
		if err := v.w.write(v.pkt); err != nil {
			v.pkt.Unref()
			return err
		}
		v.pkt.Unref()
	}
}

// flush drains the BSF (if any), then the decoder, then the encoder.
func (v *videoEncoder) flush() error {
	if v.bsf != nil {
		if err := v.bsf.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
			return fmt.Errorf("media: flush video bsf: %w", err)
		}
		if err := v.drainBSF(); err != nil {
			return err
		}
	}
	if err := v.dec.SendPacket(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return fmt.Errorf("media: flush video decoder: %w", err)
	}
	if err := v.drainDecoder(); err != nil {
		return err
	}
	return v.encode(nil)
}
