package rendition

import (
	"context"
	"errors"
	"fmt"
)

type FailureCategory string

const (
	FailureDeviceUnavailable FailureCategory = "device_unavailable"
	FailureSourceCapability  FailureCategory = "source_capability_mismatch"
	FailureResourceExhausted FailureCategory = "provider_resource_exhausted"
	FailureAdmissionTimeout  FailureCategory = "admission_timeout"
	FailureSourceIO          FailureCategory = "source_io"
	FailureCancelled         FailureCategory = "cancelled"
	FailureInvalidOutput     FailureCategory = "invalid_output"
	FailureMux               FailureCategory = "mux_failure"
)

// RenditionError preserves the stable failure category required by fallback,
// circuit-breaker, HTTP, and diagnostic policy without exposing providers.
type RenditionError struct {
	category   FailureCategory
	provider   string
	deviceID   string
	stage      string
	generation string
	cause      error
}

func newFailure(category FailureCategory, provider, deviceID, stage string, cause error) *RenditionError {
	if cause == nil {
		cause = errors.New(string(category))
	}
	return &RenditionError{
		category: category,
		provider: provider,
		deviceID: deviceID,
		stage:    stage,
		cause:    cause,
	}
}

func (e *RenditionError) Error() string {
	target := e.provider
	if e.deviceID != "" {
		target += "/" + e.deviceID
	}
	if target == "" {
		target = "video"
	}
	return fmt.Sprintf("rendition: %s: %s at %s: %v", target, e.category, e.stage, e.cause)
}

func (e *RenditionError) Unwrap() error             { return e.cause }
func (e *RenditionError) Category() FailureCategory { return e.category }
func (e *RenditionError) Provider() string          { return e.provider }
func (e *RenditionError) DeviceID() string          { return e.deviceID }
func (e *RenditionError) Stage() string             { return e.stage }
func (e *RenditionError) GenerationID() string      { return e.generation }

func FailureCategoryOf(err error) FailureCategory {
	var failure *RenditionError
	if errors.As(err, &failure) {
		return failure.category
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return FailureCancelled
	}
	return FailureInvalidOutput
}

func withGeneration(err error, generationID string) error {
	var failure *RenditionError
	if errors.As(err, &failure) {
		copy := *failure
		copy.generation = generationID
		return &copy
	}
	category := FailureInvalidOutput
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		category = FailureCancelled
	}
	wrapped := newFailure(category, "", "", "segment", err)
	wrapped.generation = generationID
	return wrapped
}

func canFallbackBeforeCommit(err error) bool {
	switch FailureCategoryOf(err) {
	case FailureDeviceUnavailable,
		FailureSourceCapability,
		FailureResourceExhausted,
		FailureAdmissionTimeout,
		FailureInvalidOutput,
		FailureMux:
		return true
	default:
		return false
	}
}
