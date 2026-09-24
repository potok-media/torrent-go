package media

import (
	"context"
	"errors"
	"testing"
)

func TestVideoTranscodeLimit(t *testing.T) {
	tests := map[string]int{
		"":    1,
		"0":   1,
		"bad": 1,
		"1":   1,
		"2":   2,
		"8":   8,
		"9":   8,
	}
	for value, want := range tests {
		if got := parseVideoTranscodeLimit(value); got != want {
			t.Fatalf("parseVideoTranscodeLimit(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestVideoTranscodeGateWaitIsCancellable(t *testing.T) {
	gate := newVideoTranscodeGate(1)
	release, err := gate.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("second acquire error = %v, want context.Canceled", err)
	}
}
