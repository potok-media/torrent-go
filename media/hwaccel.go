package media

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/asticode/go-astiav"
)

type GPUConfig struct {
	HwType      astiav.HardwareDeviceType
	HwTypeName  string
	EncoderName string
	Device      string
}

var ActiveGPU *GPUConfig

type gpuCandidate struct {
	name        string
	hwTypeName  string
	encoderName string
}

func gpuCandidates(goos string) []gpuCandidate {
	switch goos {
	case "darwin":
		return []gpuCandidate{
			{name: "videotoolbox", hwTypeName: "videotoolbox", encoderName: "h264_videotoolbox"},
		}
	case "linux":
		return []gpuCandidate{
			{name: "nvenc", hwTypeName: "cuda", encoderName: "h264_nvenc"},
			{name: "vaapi", hwTypeName: "vaapi", encoderName: "h264_vaapi"},
		}
	default:
		return nil
	}
}

// cpuEncoderThreadLimit keeps the last-resort software path usable without letting a single stream consume
// every core on a small server. Hardware encoding remains the normal path; this is deliberately conservative.
func cpuEncoderThreadLimit() int { return 2 }

func hwAccelDisabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func InitGPU() {
	ActiveGPU = nil
	if hwAccelDisabled(os.Getenv("POTOK_DISABLE_HWACCEL")) {
		slog.Info("hwaccel disabled via POTOK_DISABLE_HWACCEL")
		return
	}

	candidates := gpuCandidates(runtime.GOOS)

	for _, c := range candidates {
		hwType := astiav.FindHardwareDeviceTypeByName(c.hwTypeName)
		if hwType == astiav.HardwareDeviceTypeNone {
			slog.Debug("GPU candidate hardware type not found in FFmpeg build", "candidate", c.name)
			continue
		}

		enc := astiav.FindEncoderByName(c.encoderName)
		if enc == nil {
			slog.Debug("GPU candidate encoder codec not found in FFmpeg build", "candidate", c.name, "encoder", c.encoderName)
			continue
		}

		// Identify device path if needed (specifically VAAPI)
		device := ""
		if c.name == "vaapi" {
			device = findRenderNode()
			if device == "" {
				slog.Debug("VAAPI skipped: no DRI render node found")
				continue
			}
		}

		// Try creating hardware device context to verify hardware is functional
		hwDev, err := astiav.CreateHardwareDeviceContext(hwType, device, nil, 0)
		if err != nil {
			slog.Debug("GPU candidate failed to initialize device context", "candidate", c.name, "err", err)
			continue
		}
		if err := probeHardwareEncoder(hwDev, enc); err != nil {
			hwDev.Free()
			slog.Debug("GPU candidate failed packet-level encode probe", "candidate", c.name, "encoder", c.encoderName, "err", err)
			continue
		}
		hwDev.Free()

		slog.Info("GPU hardware video acceleration selected", "decoding", c.hwTypeName, "encoding", c.encoderName)
		ActiveGPU = &GPUConfig{
			HwType:      hwType,
			HwTypeName:  c.hwTypeName,
			EncoderName: c.encoderName,
			Device:      device,
		}
		return
	}

	slog.Info("No hardware video acceleration available, decoding and encoding via software")
}

// probeHardwareEncoder rejects drivers that can create a device context but cannot actually open an H.264
// session or accept a frame. This is intentionally done before advertising a provider: otherwise every HLS
// segment would retry the same broken GPU and fall back to CPU independently.
func probeHardwareEncoder(hwDev *astiav.HardwareDeviceContext, codec *astiav.Codec) error {
	enc := astiav.AllocCodecContext(codec)
	if enc == nil {
		return errors.New("alloc encoder")
	}
	defer enc.Free()

	const width, height = 128, 72
	enc.SetWidth(width)
	enc.SetHeight(height)
	enc.SetTimeBase(astiav.NewRational(1, 30))
	enc.SetFramerate(astiav.NewRational(30, 1))
	enc.SetProfile(astiav.ProfileH264High)
	enc.SetMaxBFrames(0)
	enc.SetHardwareDeviceContext(hwDev)

	var frames *astiav.HardwareFramesContext
	if codec.Name() == "h264_vaapi" {
		enc.SetPixelFormat(astiav.PixelFormatVaapi)
		frames = astiav.AllocHardwareFramesContext(hwDev)
		if frames == nil {
			return errors.New("alloc hardware frames context")
		}
		defer frames.Free()
		frames.SetHardwarePixelFormat(astiav.PixelFormatVaapi)
		frames.SetSoftwarePixelFormat(astiav.PixelFormatNv12)
		frames.SetWidth(width)
		frames.SetHeight(height)
		frames.SetInitialPoolSize(2)
		if err := frames.Initialize(); err != nil {
			return fmt.Errorf("initialize hardware frames context: %w", err)
		}
		enc.SetHardwareFramesContext(frames)
	} else {
		enc.SetPixelFormat(astiav.PixelFormatNv12)
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
	}
	if err := enc.Open(codec, opts); err != nil {
		return fmt.Errorf("open encoder: %w", err)
	}

	software := astiav.AllocFrame()
	if software == nil {
		return errors.New("alloc software frame")
	}
	defer software.Free()
	software.SetWidth(width)
	software.SetHeight(height)
	software.SetPixelFormat(astiav.PixelFormatNv12)
	software.SetPts(0)
	if err := software.AllocBuffer(0); err != nil {
		return fmt.Errorf("alloc software frame buffer: %w", err)
	}
	if err := software.ImageFillBlack(); err != nil {
		return fmt.Errorf("fill probe frame: %w", err)
	}

	frame := software
	var hardware *astiav.Frame
	if frames != nil {
		hardware = astiav.AllocFrame()
		if hardware == nil {
			return errors.New("alloc hardware frame")
		}
		defer hardware.Free()
		if err := hardware.AllocHardwareBuffer(frames); err != nil {
			return fmt.Errorf("alloc hardware frame buffer: %w", err)
		}
		if err := software.TransferHardwareData(hardware); err != nil {
			return fmt.Errorf("upload probe frame: %w", err)
		}
		hardware.SetPts(0)
		frame = hardware
	}

	if err := enc.SendFrame(frame); err != nil {
		return fmt.Errorf("send probe frame: %w", err)
	}
	pkt := astiav.AllocPacket()
	if pkt == nil {
		return errors.New("alloc probe packet")
	}
	defer pkt.Free()
	if err := enc.ReceivePacket(pkt); err == nil {
		return nil
	} else if !errors.Is(err, astiav.ErrEagain) {
		return fmt.Errorf("receive probe packet: %w", err)
	}
	if err := enc.SendFrame(nil); err != nil && !errors.Is(err, astiav.ErrEof) {
		return fmt.Errorf("flush probe encoder: %w", err)
	}
	if err := enc.ReceivePacket(pkt); err != nil {
		return fmt.Errorf("receive flushed probe packet: %w", err)
	}
	return nil
}

func findRenderNode() string {
	if configured := os.Getenv("POTOK_VAAPI_DEVICE"); configured != "" {
		if _, err := os.Stat(configured); err == nil {
			return configured
		}
		slog.Warn("configured VAAPI device is unavailable", "device", configured)
		return ""
	}
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	for _, m := range matches {
		if _, err := os.Stat(m); err == nil {
			return m
		}
	}
	return ""
}
