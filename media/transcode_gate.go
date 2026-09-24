package media

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
)

const maxVideoTranscodeConcurrency = 8

type videoTranscodeGate struct {
	slots chan struct{}
}

func newVideoTranscodeGate(limit int) *videoTranscodeGate {
	return &videoTranscodeGate{slots: make(chan struct{}, limit)}
}

func (g *videoTranscodeGate) acquire(ctx context.Context) (func(), error) {
	select {
	case g.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.slots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func parseVideoTranscodeLimit(value string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || limit < 1 {
		return 1
	}
	if limit > maxVideoTranscodeConcurrency {
		return maxVideoTranscodeConcurrency
	}
	return limit
}

var processVideoTranscodes = newVideoTranscodeGate(parseVideoTranscodeLimit(os.Getenv("POTOK_VIDEO_TRANSCODE_CONCURRENCY")))
