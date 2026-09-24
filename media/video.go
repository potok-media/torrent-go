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
	hwFramesCtx *astiav.HardwareFramesContext
	toneMapper  *videoToneMapper
	sws         *astiav.SoftwareScaleContext
	decFrm      *astiav.Frame
	transferFrm *astiav.Frame
	scaled      *astiav.Frame
	uploadFrm   *astiav.Frame
	pkt         *astiav.Packet
	bsf         *astiav.BitStreamFilterContext // mpeg4_unpack_bframes for DivX/Xvid packed bitstream; nil otherwise
	bsfPkt      *astiav.Packet                 // scratch for BSF output packets; nil when bsf is nil

	startTS    int64
	endTS      int64
	lastPTS    int64 // last decoded-frame pts fed to the encoder — monotonic guard (noPTS = none yet)
	lastDTS    int64 // last output-packet dts written to the muxer — monotonic guard (noPTS = none yet)
	inTimeBase astiav.Rational
	hwDecode   bool
	sourceHDR  bool
	hdr10      bool
	toneMapHW  bool
	forceSWMap bool
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
	transfer := in.CodecParameters().ColorTransferCharacteristic()

	decCodec := astiav.FindDecoder(in.CodecParameters().CodecID())
	if decCodec == nil {
		return nil, fmt.Errorf("media: no video decoder for %s", in.CodecParameters().CodecID().String())
	}
	var hwDevCtx *astiav.HardwareDeviceContext
	if ActiveGPU != nil {
		var err error
		hwDevCtx, err = astiav.CreateHardwareDeviceContext(ActiveGPU.HwType, ActiveGPU.Device, nil, 0)
		if err != nil {
			slog.Warn("hardware video device disappeared; using CPU for this rendition", "provider", ActiveGPU.HwTypeName, "err", err)
			hwDevCtx = nil
		}
	}

	dec, hwDecode, err := openVideoDecoder(in, decCodec, hwDevCtx)
	if err != nil {
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, err
	}

	enc, encCodec, hwFramesCtx, err := openVideoEncoder(in, dec, ofc, hwDevCtx)
	if err != nil {
		dec.Free()
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, err
	}

	out := ofc.NewStream(nil)
	if out == nil {
		dec.Free()
		enc.Free()
		if hwFramesCtx != nil {
			hwFramesCtx.Free()
		}
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: new video output stream")
	}
	if err := out.CodecParameters().FromCodecContext(enc); err != nil {
		dec.Free()
		enc.Free()
		if hwFramesCtx != nil {
			hwFramesCtx.Free()
		}
		if hwDevCtx != nil {
			hwDevCtx.Free()
		}
		return nil, fmt.Errorf("media: video encoder params -> stream: %w", err)
	}
	out.SetTimeBase(enc.TimeBase())

	bsf, bsfPkt := mpeg4UnpackBSF(in)
	var transferFrm *astiav.Frame
	if hwDecode {
		transferFrm = astiav.AllocFrame()
	}
	var uploadFrm *astiav.Frame
	if hwFramesCtx != nil {
		uploadFrm = astiav.AllocFrame()
	}
	slog.Info("video transcoder opened", "decoder", decCodec.Name(), "hardwareDecode", hwDecode, "encoder", encCodec.Name())

	return &videoEncoder{
		ofc:         ofc,
		outIdx:      out.Index(),
		w:           fw,
		dec:         dec,
		enc:         enc,
		hwDevCtx:    hwDevCtx,
		hwFramesCtx: hwFramesCtx,
		inTimeBase:  in.TimeBase(),
		decFrm:      astiav.AllocFrame(),
		transferFrm: transferFrm,
		scaled:      astiav.AllocFrame(),
		uploadFrm:   uploadFrm,
		pkt:         astiav.AllocPacket(),
		bsf:         bsf,
		bsfPkt:      bsfPkt,
		startTS:     startTS,
		endTS:       endTS,
		lastPTS:     noPTS,
		lastDTS:     noPTS,
		hwDecode:    hwDecode,
		sourceHDR:   isHDRTransfer(transfer),
		hdr10:       transfer == astiav.ColorTransferCharacteristicSmpte2084,
	}, nil
}

func openVideoDecoder(in *astiav.Stream, codec *astiav.Codec, hwDevCtx *astiav.HardwareDeviceContext) (*astiav.CodecContext, bool, error) {
	newContext := func() (*astiav.CodecContext, error) {
		ctx := astiav.AllocCodecContext(codec)
		if ctx == nil {
			return nil, fmt.Errorf("media: alloc video decoder")
		}
		if err := in.CodecParameters().ToCodecContext(ctx); err != nil {
			ctx.Free()
			return nil, fmt.Errorf("media: video decoder params: %w", err)
		}
		return ctx, nil
	}

	if ActiveGPU != nil && hwDevCtx != nil {
		for _, cfg := range codec.HardwareConfigs() {
			if cfg.HardwareDeviceType() != ActiveGPU.HwType || !cfg.MethodFlags().Has(astiav.CodecHardwareConfigMethodFlagHwDeviceCtx) {
				continue
			}
			hwPixFmt := cfg.PixelFormat()
			ctx, err := newContext()
			if err != nil {
				return nil, false, err
			}
			ctx.SetHardwareDeviceContext(hwDevCtx)
			ctx.SetPixelFormatCallback(func(pfs []astiav.PixelFormat) astiav.PixelFormat {
				for _, pf := range pfs {
					if pf == hwPixFmt {
						return pf
					}
				}
				return astiav.PixelFormatNone
			})
			if err := ctx.Open(codec, nil); err == nil {
				return ctx, true, nil
			} else {
				slog.Warn("hardware decoder rejected source; retaining hardware encoder with software decode", "provider", ActiveGPU.HwTypeName, "codec", codec.Name(), "err", err)
				ctx.Free()
			}
			break
		}
	}

	ctx, err := newContext()
	if err != nil {
		return nil, false, err
	}
	ctx.SetThreadCount(cpuEncoderThreadLimit())
	if err := ctx.Open(codec, nil); err != nil {
		ctx.Free()
		return nil, false, fmt.Errorf("media: open software video decoder: %w", err)
	}
	return ctx, false, nil
}

func openVideoEncoder(in *astiav.Stream, dec *astiav.CodecContext, ofc *astiav.FormatContext, hwDevCtx *astiav.HardwareDeviceContext) (*astiav.CodecContext, *astiav.Codec, *astiav.HardwareFramesContext, error) {
	if ActiveGPU != nil && hwDevCtx != nil {
		codec := astiav.FindEncoderByName(ActiveGPU.EncoderName)
		if codec != nil {
			enc, frames, err := openVideoEncoderCandidate(in, dec, ofc, codec, hwDevCtx)
			if err == nil {
				return enc, codec, frames, nil
			}
			slog.Warn("hardware encoder could not open; using bounded CPU fallback", "provider", ActiveGPU.HwTypeName, "encoder", ActiveGPU.EncoderName, "err", err)
		}
	}

	codec := astiav.FindEncoderByName("libx264")
	if codec == nil {
		codec = astiav.FindEncoder(astiav.CodecIDH264)
	}
	if codec == nil {
		return nil, nil, nil, fmt.Errorf("media: no H.264 encoder available")
	}
	enc, _, err := openVideoEncoderCandidate(in, dec, ofc, codec, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("media: open CPU H.264 encoder: %w", err)
	}
	return enc, codec, nil, nil
}

func openVideoEncoderCandidate(in *astiav.Stream, dec *astiav.CodecContext, ofc *astiav.FormatContext, codec *astiav.Codec, hwDevCtx *astiav.HardwareDeviceContext) (*astiav.CodecContext, *astiav.HardwareFramesContext, error) {
	enc := astiav.AllocCodecContext(codec)
	if enc == nil {
		return nil, nil, fmt.Errorf("media: alloc H.264 encoder %s", codec.Name())
	}
	fail := func(frames *astiav.HardwareFramesContext, err error) (*astiav.CodecContext, *astiav.HardwareFramesContext, error) {
		if frames != nil {
			frames.Free()
		}
		enc.Free()
		return nil, nil, err
	}

	enc.SetWidth(dec.Width())
	enc.SetHeight(dec.Height())
	enc.SetSampleAspectRatio(dec.SampleAspectRatio())
	enc.SetProfile(astiav.ProfileH264High)
	enc.SetMaxBFrames(0)
	if isHDRTransfer(in.CodecParameters().ColorTransferCharacteristic()) {
		enc.SetColorPrimaries(astiav.ColorPrimariesBt709)
		enc.SetColorTransferCharacteristic(astiav.ColorTransferCharacteristicBt709)
		enc.SetColorSpace(astiav.ColorSpaceBt709)
		enc.SetColorRange(astiav.ColorRangeMpeg)
	} else {
		enc.SetColorPrimaries(in.CodecParameters().ColorPrimaries())
		enc.SetColorTransferCharacteristic(in.CodecParameters().ColorTransferCharacteristic())
		enc.SetColorSpace(in.CodecParameters().ColorSpace())
		enc.SetColorRange(in.CodecParameters().ColorRange())
	}

	fps := in.AvgFrameRate()
	if fps.Num() > 0 && fps.Den() > 0 {
		enc.SetFramerate(fps)
		enc.SetTimeBase(fps.Invert())
	} else {
		enc.SetTimeBase(in.TimeBase())
	}
	if ofc.OutputFormat().Flags().Has(astiav.IOFormatFlagGlobalheader) {
		enc.SetFlags(enc.Flags().Add(astiav.CodecContextFlagGlobalHeader))
	}

	var frames *astiav.HardwareFramesContext
	switch codec.Name() {
	case "h264_vaapi":
		enc.SetPixelFormat(astiav.PixelFormatVaapi)
		enc.SetHardwareDeviceContext(hwDevCtx)
		frames = astiav.AllocHardwareFramesContext(hwDevCtx)
		if frames == nil {
			return fail(nil, fmt.Errorf("media: alloc VAAPI frames context"))
		}
		frames.SetHardwarePixelFormat(astiav.PixelFormatVaapi)
		frames.SetSoftwarePixelFormat(astiav.PixelFormatNv12)
		frames.SetWidth(dec.Width())
		frames.SetHeight(dec.Height())
		frames.SetInitialPoolSize(20)
		if err := frames.Initialize(); err != nil {
			return fail(frames, fmt.Errorf("media: initialize VAAPI frames context: %w", err))
		}
		enc.SetHardwareFramesContext(frames)
	case "h264_nvenc", "h264_videotoolbox":
		enc.SetPixelFormat(astiav.PixelFormatNv12)
		enc.SetHardwareDeviceContext(hwDevCtx)
	default:
		enc.SetPixelFormat(astiav.PixelFormatYuv420P)
		enc.SetThreadCount(cpuEncoderThreadLimit())
	}

	opts := astiav.NewDictionary()
	defer opts.Free()
	switch codec.Name() {
	case "h264_nvenc":
		_ = opts.Set("preset", "p4", 0)
		_ = opts.Set("tune", "ll", 0)
		_ = opts.Set("rc", "vbr", 0)
		_ = opts.Set("cq", "24", 0)
	case "h264_vaapi":
		_ = opts.Set("rc_mode", "CQP", 0)
		_ = opts.Set("qp", "24", 0)
	case "h264_videotoolbox":
		_ = opts.Set("realtime", "1", 0)
		_ = opts.Set("allow_sw", "0", 0)
	default:
		_ = opts.Set("preset", "veryfast", 0)
		_ = opts.Set("crf", "23", 0)
		_ = opts.Set("tune", "zerolatency", 0)
	}
	if err := enc.Open(codec, opts); err != nil {
		return fail(frames, fmt.Errorf("media: open H.264 encoder %s: %w", codec.Name(), err))
	}
	return enc, frames, nil
}

func (v *videoEncoder) free() {
	v.pkt.Free()
	if v.bsfPkt != nil {
		v.bsfPkt.Free()
	}
	if v.bsf != nil {
		v.bsf.Free()
	}
	if v.toneMapper != nil {
		v.toneMapper.free()
	}
	v.scaled.Free()
	if v.uploadFrm != nil {
		v.uploadFrm.Free()
	}
	if v.transferFrm != nil {
		v.transferFrm.Free()
	}
	v.decFrm.Free()
	if v.sws != nil {
		v.sws.Free()
	}
	v.enc.Free()
	v.dec.Free()
	if v.hwFramesCtx != nil {
		v.hwFramesCtx.Free()
	}
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

// encodeDecoded normalizes decoded video to an 8-bit browser-safe format. VAAPI requires an explicit
// software NV12 -> hardware-frame upload; passing a software frame to an encoder configured for the VAAPI
// pixel format is invalid and was the reason the old path silently stayed on libx264.
func (v *videoEncoder) encodeDecoded() error {
	if v.canHardwareToneMap() && !v.forceSWMap {
		if v.toneMapper == nil {
			mapper, err := newVideoToneMapper(v.decFrm, v.inTimeBase, vaapiToneMapFilter(), v.hwDevCtx, true)
			if err == nil {
				v.toneMapper = mapper
				v.toneMapHW = true
			} else {
				slog.Warn("VAAPI HDR tone mapping unavailable; using bounded software tone mapping with hardware encode", "err", err)
				v.forceSWMap = true
			}
		}
		if v.toneMapper != nil {
			if err := v.toneMapper.process(v.decFrm, v.encodeReadyFrame); err == nil {
				return nil
			} else {
				slog.Warn("VAAPI HDR tone mapping failed; using bounded software tone mapping with hardware encode", "err", err)
				v.toneMapper.free()
				v.toneMapper = nil
				v.toneMapHW = false
				v.forceSWMap = true
			}
		}
	}

	frame := v.decFrm
	if v.hwDecode {
		v.transferFrm.Unref()
		if err := v.decFrm.TransferHardwareData(v.transferFrm); err != nil {
			return fmt.Errorf("media: download decoded hardware frame: %w", err)
		}
		v.transferFrm.SetPts(v.decFrm.Pts())
		frame = v.transferFrm
	}

	targetFmt := v.enc.PixelFormat()
	if v.hwFramesCtx != nil {
		targetFmt = astiav.PixelFormatNv12
	}
	if v.sourceHDR {
		if v.toneMapper == nil {
			mapper, err := newVideoToneMapper(frame, v.inTimeBase, softwareToneMapFilter(targetFmt), nil, false)
			if err != nil {
				return fmt.Errorf("media: initialize portable HDR tone mapping: %w", err)
			}
			v.toneMapper = mapper
			v.toneMapHW = false
		}
		return v.toneMapper.process(frame, v.prepareSoftwareFrame)
	}

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
	return v.prepareSoftwareFrame(frame)
}

func (v *videoEncoder) canHardwareToneMap() bool {
	return v.sourceHDR && v.hdr10 && v.hwDecode && v.hwFramesCtx != nil &&
		ActiveGPU != nil && ActiveGPU.HwTypeName == "vaapi"
}

func (v *videoEncoder) prepareSoftwareFrame(frame *astiav.Frame) error {
	if v.hwFramesCtx != nil {
		v.uploadFrm.Unref()
		if err := v.uploadFrm.AllocHardwareBuffer(v.hwFramesCtx); err != nil {
			return fmt.Errorf("media: allocate VAAPI frame: %w", err)
		}
		if err := frame.TransferHardwareData(v.uploadFrm); err != nil {
			return fmt.Errorf("media: upload frame to VAAPI: %w", err)
		}
		v.uploadFrm.SetPts(frame.Pts())
		frame = v.uploadFrm
	}
	return v.encodeReadyFrame(frame)
}

func (v *videoEncoder) encodeReadyFrame(frame *astiav.Frame) error {
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
	if v.toneMapper != nil {
		consume := v.prepareSoftwareFrame
		if v.toneMapHW {
			consume = v.encodeReadyFrame
		}
		if err := v.toneMapper.flush(consume); err != nil {
			return err
		}
	}
	return v.encode(nil)
}
