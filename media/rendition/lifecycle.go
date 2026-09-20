package rendition

import "fmt"

type generationState uint8

const (
	generationDiscovered generationState = iota
	generationCandidatePrepared
	generationPrimed
	generationCommitted
	generationRunning
	generationComplete
	generationFailed
)

func (s generationState) String() string {
	switch s {
	case generationDiscovered:
		return "discovered"
	case generationCandidatePrepared:
		return "candidate_prepared"
	case generationPrimed:
		return "primed"
	case generationCommitted:
		return "committed"
	case generationRunning:
		return "running"
	case generationComplete:
		return "complete"
	case generationFailed:
		return "failed"
	default:
		return "unknown"
	}
}

type generationLifecycle struct {
	state generationState
}

func newGenerationLifecycle() generationLifecycle {
	return generationLifecycle{state: generationDiscovered}
}

func (l *generationLifecycle) transition(next generationState) error {
	valid := false
	switch l.state {
	case generationDiscovered:
		valid = next == generationCandidatePrepared
	case generationCandidatePrepared:
		valid = next == generationPrimed
	case generationPrimed:
		valid = next == generationCommitted
	case generationCommitted:
		valid = next == generationRunning || next == generationFailed
	case generationRunning:
		valid = next == generationComplete || next == generationFailed
	}
	if !valid {
		return fmt.Errorf("rendition: invalid generation transition %s -> %s", l.state, next)
	}
	l.state = next
	return nil
}
