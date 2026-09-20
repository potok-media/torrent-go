// Package rendition owns selection and lifecycle of generation-safe HLS video
// renditions. Callers intentionally do not see codec-provider details.
package rendition

import (
	"context"
	"time"
)

// VideoRenditionManager selects and primes a complete video pipeline before it
// exposes a rendition to a caller.
type VideoRenditionManager interface {
	Acquire(context.Context, RenditionRequest) (VideoRendition, error)
}

// VideoRendition is one immutable HLS video generation.
type VideoRendition interface {
	Descriptor() RenditionDescriptor
	Init(context.Context) ([]byte, error)
	Segment(context.Context, int) ([]byte, error)
	Release()
}

type RenditionRequest struct {
	Source InputSpec
	Output OutputContract
}

type InputSpec struct {
	SourceID string
	Codec    string
	Profile  string
	Width    int
	Height   int
	BitDepth int
	HDR      bool
}

type Codec string

const CodecH264 Codec = "h264"

// Rational is a comparable value so an OutputContract has no mutable aliases.
type Rational struct {
	Num int
	Den int
}

type RateControl struct {
	TargetBitrate int64
	MaxBitrate    int64
}

type GOPContract struct {
	SegmentDuration Rational
	Closed          bool
	IDRAtBoundary   bool
}

type ColorContract struct {
	Primaries string
	Transfer  string
	Matrix    string
	Range     string
}

// OutputContract is copied by value into a committed generation. It contains
// only comparable scalar values, so callers cannot mutate it through aliases.
type OutputContract struct {
	Codec       Codec
	CodecString string
	Profile     string
	Level       string
	Width       int
	Height      int
	PixelFormat string
	FrameRate   Rational
	TimeBase    Rational
	RateControl RateControl
	GOP         GOPContract
	Color       ColorContract
}

// RenditionDescriptor is a by-value snapshot of the generation's frozen
// output. Provider and device identity are diagnostic facts, not selection
// controls for callers.
type RenditionDescriptor struct {
	GenerationID         string
	Output               OutputContract
	Bandwidth            int64
	Provider             string
	DeviceID             string
	ExtradataFingerprint string
}

type DecodeMode string

const (
	DecodeModeCopy     DecodeMode = "copy"
	DecodeModeHardware DecodeMode = "hardware"
	DecodeModeSoftware DecodeMode = "software"
)

type providerKind uint8

const (
	providerCopy providerKind = iota
	providerHardware
	providerCPU
)

type device struct {
	ID               string
	PerformanceScore float64
}

type videoProvider interface {
	Name() string
	Kind() providerKind
	Inventory(context.Context) []device
	Prepare(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error)
}

// preparedPipeline has already processed a real source frame. PrimedOutput is
// consequently safe to publish if it satisfies the requested contract.
type preparedPipeline interface {
	PrimedOutput() pipelineOutput
	Segment(context.Context, int) ([]byte, error)
	Close() error
}

type pipelineOutput struct {
	Contract             OutputContract
	Init                 []byte
	ExtradataFingerprint string
}

type ManagerOptions struct {
	// DeviceOrder is an optional ordered hardware allowlist. Entries use the
	// stable "provider/device" identity (for example "vaapi/renderD128").
	DeviceOrder []string

	// PerDeviceConcurrency applies independently to each physical hardware
	// device. CPUConcurrency is process-wide across all software adapters.
	PerDeviceConcurrency int
	CPUConcurrency       int
	CPUAdmissionWait     time.Duration
}
