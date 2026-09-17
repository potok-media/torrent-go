package handlers

import (
	"context"
	"sync"
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

	for e.inflight[cls] >= limit {
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
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
