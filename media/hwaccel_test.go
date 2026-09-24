package media

import "testing"

func TestGPUCandidatesPreferNativeHardwareEncoders(t *testing.T) {
	tests := []struct {
		goos string
		want []gpuCandidate
	}{
		{
			goos: "linux",
			want: []gpuCandidate{
				{name: "nvenc", hwTypeName: "cuda", encoderName: "h264_nvenc"},
				{name: "vaapi", hwTypeName: "vaapi", encoderName: "h264_vaapi"},
			},
		},
		{
			goos: "darwin",
			want: []gpuCandidate{
				{name: "videotoolbox", hwTypeName: "videotoolbox", encoderName: "h264_videotoolbox"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			got := gpuCandidates(tt.goos)
			if len(got) != len(tt.want) {
				t.Fatalf("gpuCandidates(%q) returned %d candidates, want %d", tt.goos, len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("gpuCandidates(%q)[%d] = %+v, want %+v", tt.goos, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCPUFallbackThreadLimit(t *testing.T) {
	if got := cpuEncoderThreadLimit(); got != 2 {
		t.Fatalf("cpuEncoderThreadLimit() = %d, want 2", got)
	}
}

func TestHardwareDisableFlagTreatsZeroAsEnabled(t *testing.T) {
	for _, value := range []string{"", "0", "false", "no", "off"} {
		if hwAccelDisabled(value) {
			t.Fatalf("hwAccelDisabled(%q) = true, want false", value)
		}
	}
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		if !hwAccelDisabled(value) {
			t.Fatalf("hwAccelDisabled(%q) = false, want true", value)
		}
	}
}
