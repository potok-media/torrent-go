package media

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/asticode/go-astiav"
)

type GPUConfig struct {
	HwType      astiav.HardwareDeviceType
	HwTypeName  string
	EncoderName string
}

var ActiveGPU *GPUConfig

func InitGPU() {
	if os.Getenv("POTOK_DISABLE_HWACCEL") != "" {
		slog.Info("hwaccel disabled via POTOK_DISABLE_HWACCEL")
		return
	}

	type candidate struct {
		name        string
		hwTypeName  string
		encoderName string
	}

	var candidates []candidate
	if runtime.GOOS == "darwin" {
		candidates = []candidate{
			{name: "videotoolbox", hwTypeName: "videotoolbox", encoderName: "h264_videotoolbox"},
		}
	} else {
		candidates = []candidate{
			{name: "nvenc", hwTypeName: "cuda", encoderName: "h264_nvenc"},
			{name: "vaapi", hwTypeName: "vaapi", encoderName: "libx264"},
		}
	}

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
		hwDev.Free()

		slog.Info("GPU hardware video acceleration selected", "decoding", c.hwTypeName, "encoding", c.encoderName)
		ActiveGPU = &GPUConfig{
			HwType:      hwType,
			HwTypeName:  c.hwTypeName,
			EncoderName: c.encoderName,
		}
		return
	}

	slog.Info("No hardware video acceleration available, decoding and encoding via software")
}

func findRenderNode() string {
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	for _, m := range matches {
		if _, err := os.Stat(m); err == nil {
			return m
		}
	}
	return ""
}
