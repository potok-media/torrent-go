package rendition

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errAdmissionWaitExpired = errors.New("admission wait expired")
var errAdmissionBusy = errors.New("admission capacity is busy")

type admissionGate struct {
	tokens chan struct{}
}

func newAdmissionGate(limit int) *admissionGate {
	return &admissionGate{tokens: make(chan struct{}, limit)}
}

func (g *admissionGate) acquire(ctx context.Context, wait time.Duration) (func(), error) {
	if wait <= 0 {
		select {
		case g.tokens <- struct{}{}:
			return g.release, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g.tokens <- struct{}{}:
		return g.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errAdmissionWaitExpired
	}
}

func (g *admissionGate) tryAcquire() (func(), error) {
	select {
	case g.tokens <- struct{}{}:
		return g.release, nil
	default:
		return nil, errAdmissionBusy
	}
}

func (g *admissionGate) release()   { <-g.tokens }
func (g *admissionGate) inUse() int { return len(g.tokens) }

type admissionRegistry struct {
	mu    sync.Mutex
	gates map[string]*admissionGate
}

func (r *admissionRegistry) gate(key string, limit int) *admissionGate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gates == nil {
		r.gates = make(map[string]*admissionGate)
	}
	if gate := r.gates[key]; gate != nil {
		return gate
	}
	gate := newAdmissionGate(limit)
	r.gates[key] = gate
	return gate
}
