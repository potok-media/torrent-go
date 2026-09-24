package media

import (
	"strings"
	"testing"

	"github.com/asticode/go-astiav"
)

func TestHDRTransferDetection(t *testing.T) {
	for _, transfer := range []astiav.ColorTransferCharacteristic{
		astiav.ColorTransferCharacteristicSmpte2084,
		astiav.ColorTransferCharacteristicAribStdB67,
	} {
		if !isHDRTransfer(transfer) {
			t.Fatalf("isHDRTransfer(%v) = false, want true", transfer)
		}
	}
	if isHDRTransfer(astiav.ColorTransferCharacteristicBt709) {
		t.Fatal("BT.709 was classified as HDR")
	}
}

func TestToneMapFiltersProduceBT709EightBitOutput(t *testing.T) {
	vaapi := vaapiToneMapFilter()
	for _, want := range []string{"tonemap_vaapi", "format=nv12", "p=bt709", "t=bt709", "m=bt709"} {
		if !strings.Contains(vaapi, want) {
			t.Fatalf("VAAPI tone-map filter %q does not contain %q", vaapi, want)
		}
	}

	software := softwareToneMapFilter(astiav.PixelFormatYuv420P)
	for _, want := range []string{"zscale=t=linear", "format=gbrpf32le", "tonemap=tonemap=hable", "p=bt709", "t=bt709", "m=bt709", "format=pix_fmts=yuv420p"} {
		if !strings.Contains(software, want) {
			t.Fatalf("software tone-map filter %q does not contain %q", software, want)
		}
	}
}

func TestToneMapFiltersAreIncludedInFFmpegBuild(t *testing.T) {
	for _, name := range []string{"zscale", "tonemap", "tonemap_vaapi"} {
		if astiav.FindFilterByName(name) == nil {
			t.Fatalf("required FFmpeg filter %q is unavailable", name)
		}
	}
}
