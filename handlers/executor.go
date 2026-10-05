package handlers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// extClass is the kind of ffmpeg extraction competing for the box's CPU/decode + torrent bandwidth.
// One admission controller (extExecutor) with a per-class limit replaces the pile of ad-hoc, unrelated
// channel semaphores (subtitleSem/subtitleWinSem/thumbnailSem/analyzer-local) that said nothing about
// the shared scarce resource and could each leak a slot across a starvable read.
type extClass int

const (
	extWindow  extClass = iota // cheap seek+read of one time window (subtitle window, thumbnail)
	extHeavy                   // full-file demux (batch/single subtitle) — expensive, patient
	extAnalyze                 // analyzer fingerprint decode
)

func (c extClass) String() string {
	switch c {
	case extWindow:
		return "window"
	case extHeavy:
		return "heavy"
	case extAnalyze:
		return "analyze"
	default:
		return "unknown"
	}
}

// extExecutor admits extraction jobs up to a per-class limit, blocking (ctx-cancellable) when full.
type extExecutor struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inflight map[extClass]int
	limits   map[extClass]int
}

func newExtExecutor(window, heavy, analyze int) *extExecutor {
	e := &extExecutor{
		inflight: make(map[extClass]int),
		limits:   map[extClass]int{extWindow: window, extHeavy: heavy, extAnalyze: analyze},
	}
	e.cond = sync.NewCond(&e.mu)
	return e
}

// Acquire blocks until a slot for cls is free or ctx is done. It returns a release func that is safe to
// call exactly once (idempotent via sync.Once) — always defer it. A non-positive limit means unlimited.
func (e *extExecutor) Acquire(ctx context.Context, cls extClass) (func(), error) {
	e.mu.Lock()
	limit := e.limits[cls]
	if limit <= 0 {
		e.mu.Unlock()
		return func() {}, nil
	}

	// Cond can't select on ctx, so wake the waiter when ctx fires (Go 1.21 context.AfterFunc).
	stop := context.AfterFunc(ctx, func() {
		e.mu.Lock()
		e.cond.Broadcast()
		e.mu.Unlock()
	})
	defer stop()

	waitStart := time.Now()
	for e.inflight[cls] >= limit {
		if err := ctx.Err(); err != nil {
			waited := time.Since(waitStart)
			e.mu.Unlock()
			// A deadline means the queue genuinely starved the job (worth a warn); a plain cancel is
			// just the client leaving mid-wait (routine, debug).
			if errors.Is(err, context.DeadlineExceeded) {
				slog.Warn("extraction admission timed out", "class", cls.String(), "waited_ms", waited.Milliseconds(), "limit", limit)
			} else {
				slog.Debug("extraction admission canceled", "class", cls.String(), "waited_ms", waited.Milliseconds())
			}
			return nil, err
		}
		e.cond.Wait()
	}
	e.inflight[cls]++
	e.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			e.inflight[cls]--
			e.cond.Broadcast()
			e.mu.Unlock()
		})
	}, nil
}
