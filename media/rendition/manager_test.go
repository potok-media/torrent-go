package rendition

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireReturnsPrimedCommittedRendition(t *testing.T) {
	contract := testOutputContract()
	provider := &fakeProvider{
		name:    "vaapi",
		kind:    providerHardware,
		devices: []device{{ID: "renderD128", PerformanceScore: 10}},
		prepare: func(_ context.Context, gotDevice device, _ InputSpec, gotContract OutputContract, mode DecodeMode) (preparedPipeline, error) {
			if gotDevice.ID != "renderD128" {
				t.Fatalf("device = %q, want renderD128", gotDevice.ID)
			}
			if mode != DecodeModeHardware {
				t.Fatalf("decode mode = %q, want hardware", mode)
			}
			if gotContract != contract {
				t.Fatalf("output contract changed during acquire")
			}
			return &fakePipeline{
				output: pipelineOutput{
					Contract:             contract,
					Init:                 []byte("ftyp-primed-init"),
					ExtradataFingerprint: "sha256:codec-config",
				},
				segments: map[int][]byte{0: []byte("segment-0")},
			}, nil
		},
	}

	manager := newVideoRenditionManager(ManagerOptions{}, provider)
	r, err := manager.Acquire(context.Background(), RenditionRequest{
		Source: InputSpec{SourceID: "movie:0", Width: 1920, Height: 1080},
		Output: contract,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(r.Release)

	descriptor := r.Descriptor()
	if descriptor.GenerationID == "" {
		t.Fatal("Descriptor().GenerationID is empty")
	}
	if descriptor.Provider != "vaapi" || descriptor.DeviceID != "renderD128" {
		t.Fatalf("selected %s/%s, want vaapi/renderD128", descriptor.Provider, descriptor.DeviceID)
	}
	if descriptor.Output != contract {
		t.Fatalf("Descriptor().Output changed after commit")
	}
	if descriptor.ExtradataFingerprint != "sha256:codec-config" {
		t.Fatalf("extradata fingerprint = %q", descriptor.ExtradataFingerprint)
	}

	init, err := r.Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if !bytes.Equal(init, []byte("ftyp-primed-init")) {
		t.Fatalf("Init() = %q", init)
	}
	init[0] = 'X'
	initAgain, err := r.Init(context.Background())
	if err != nil {
		t.Fatalf("second Init() error = %v", err)
	}
	if !bytes.Equal(initAgain, []byte("ftyp-primed-init")) {
		t.Fatalf("caller mutated committed init through an alias: %q", initAgain)
	}
}

func TestAcquireFallsBackWithinHardwareBeforeCPU(t *testing.T) {
	contract := testOutputContract()
	hardware := &fakeProvider{
		name:    "vaapi",
		kind:    providerHardware,
		devices: []device{{ID: "renderD128", PerformanceScore: 10}},
		prepare: func(_ context.Context, _ device, _ InputSpec, _ OutputContract, mode DecodeMode) (preparedPipeline, error) {
			switch mode {
			case DecodeModeHardware:
				return nil, newFailure(FailureSourceCapability, "vaapi", "renderD128", "prepare", errFakeUnsupported)
			case DecodeModeSoftware:
				return &fakePipeline{output: pipelineOutput{
					Contract:             contract,
					Init:                 []byte("uploaded-init"),
					ExtradataFingerprint: "sha256:uploaded",
				}}, nil
			default:
				t.Fatalf("unexpected decode mode %q", mode)
				return nil, nil
			}
		},
	}
	cpu := &fakeProvider{
		name:    "cpu",
		kind:    providerCPU,
		devices: []device{{ID: "cpu"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			t.Fatal("CPU provider was reached before software-decode hardware-encode candidate")
			return nil, nil
		},
	}

	// Registration order deliberately puts CPU first. Policy order must win.
	manager := newVideoRenditionManager(ManagerOptions{}, cpu, hardware)
	r, err := manager.Acquire(context.Background(), RenditionRequest{
		Source: InputSpec{SourceID: "movie:hevc", Codec: "hevc", Width: 1920, Height: 1080},
		Output: contract,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(r.Release)

	if got := r.Descriptor().Provider; got != "vaapi" {
		t.Fatalf("selected provider = %q, want vaapi", got)
	}
	if got, want := hardware.prepared, []DecodeMode{DecodeModeHardware, DecodeModeSoftware}; !slicesEqual(got, want) {
		t.Fatalf("VAAPI modes = %v, want %v", got, want)
	}
}

func TestAcquireHonorsExplicitDeviceOrderOverPerformanceScore(t *testing.T) {
	contract := testOutputContract()
	pipelineFor := func(fingerprint string) preparedPipeline {
		return &fakePipeline{output: pipelineOutput{
			Contract:             contract,
			Init:                 []byte("init"),
			ExtradataFingerprint: fingerprint,
		}}
	}
	vaapi := &fakeProvider{
		name:    "vaapi",
		kind:    providerHardware,
		devices: []device{{ID: "renderD128", PerformanceScore: 10}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			return pipelineFor("sha256:vaapi"), nil
		},
	}
	nvenc := &fakeProvider{
		name:    "nvenc",
		kind:    providerHardware,
		devices: []device{{ID: "gpu0", PerformanceScore: 100}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			return pipelineFor("sha256:nvenc"), nil
		},
	}

	manager := newVideoRenditionManager(ManagerOptions{
		DeviceOrder: []string{"vaapi/renderD128", "nvenc/gpu0"},
	}, nvenc, vaapi)
	r, err := manager.Acquire(context.Background(), RenditionRequest{
		Source: InputSpec{SourceID: "movie:0", Width: 1920, Height: 1080},
		Output: contract,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(r.Release)

	if got := r.Descriptor().Provider + "/" + r.Descriptor().DeviceID; got != "vaapi/renderD128" {
		t.Fatalf("selected device = %q, want explicit first device vaapi/renderD128", got)
	}
}

func TestCommittedGenerationFailureNeverFallsBackInPlace(t *testing.T) {
	contract := testOutputContract()
	pipeline := &fakePipeline{
		output: pipelineOutput{
			Contract:             contract,
			Init:                 []byte("hardware-init"),
			ExtradataFingerprint: "sha256:hardware",
		},
		segment: func(context.Context, int) ([]byte, error) {
			return nil, newFailure(FailureDeviceUnavailable, "vaapi", "renderD128", "segment", errFakeDeviceLost)
		},
	}
	hardware := &fakeProvider{
		name:    "vaapi",
		kind:    providerHardware,
		devices: []device{{ID: "renderD128", PerformanceScore: 10}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			return pipeline, nil
		},
	}
	cpu := &fakeProvider{
		name:    "cpu",
		kind:    providerCPU,
		devices: []device{{ID: "cpu"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			t.Fatal("post-commit failure attempted an in-place CPU fallback")
			return nil, nil
		},
	}
	manager := newVideoRenditionManager(ManagerOptions{}, hardware, cpu)
	r, err := manager.Acquire(context.Background(), RenditionRequest{
		Source: InputSpec{SourceID: "movie:0", Width: 1920, Height: 1080},
		Output: contract,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(r.Release)
	generationID := r.Descriptor().GenerationID

	_, firstErr := r.Segment(context.Background(), 0)
	_, secondErr := r.Segment(context.Background(), 0)
	if FailureCategoryOf(firstErr) != FailureDeviceUnavailable {
		t.Fatalf("first segment category = %q", FailureCategoryOf(firstErr))
	}
	if !errors.Is(firstErr, errFakeDeviceLost) || !errors.Is(secondErr, errFakeDeviceLost) {
		t.Fatalf("terminal cause was not preserved: first=%v second=%v", firstErr, secondErr)
	}
	var failure *RenditionError
	if !errors.As(firstErr, &failure) {
		t.Fatalf("segment error type = %T, want *RenditionError", firstErr)
	}
	if failure.GenerationID() != generationID {
		t.Fatalf("failure generation = %q, want %q", failure.GenerationID(), generationID)
	}
	if pipeline.segmentCalls != 1 {
		t.Fatalf("failed pipeline called %d times, want once", pipeline.segmentCalls)
	}
}

func TestSegmentCancellationDoesNotFailCommittedGeneration(t *testing.T) {
	for _, requestErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(requestErr.Error(), func(t *testing.T) {
			contract := testOutputContract()
			pipeline := &fakePipeline{
				output: pipelineOutput{
					Contract:             contract,
					Init:                 []byte("hardware-init"),
					ExtradataFingerprint: "sha256:hardware",
				},
				segment: func(_ context.Context, _ int) ([]byte, error) {
					if requestErr != nil {
						err := requestErr
						requestErr = nil
						return nil, err
					}
					return []byte("segment-after-retry"), nil
				},
			}
			provider := &fakeProvider{
				name:    "vaapi",
				kind:    providerHardware,
				devices: []device{{ID: "renderD128"}},
				prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
					return pipeline, nil
				},
			}
			manager := newVideoRenditionManager(ManagerOptions{}, provider)
			r, err := manager.Acquire(context.Background(), RenditionRequest{
				Source: InputSpec{SourceID: "movie:0"},
				Output: contract,
			})
			if err != nil {
				t.Fatalf("Acquire() error = %v", err)
			}
			defer r.Release()

			_, err = r.Segment(context.Background(), 0)
			if FailureCategoryOf(err) != FailureCancelled {
				t.Fatalf("first Segment() category = %q, want %q (err=%v)", FailureCategoryOf(err), FailureCancelled, err)
			}
			var failure *RenditionError
			if !errors.As(err, &failure) || failure.GenerationID() != r.Descriptor().GenerationID {
				t.Fatalf("first Segment() must return a generation-scoped RenditionError: %v", err)
			}

			segment, err := r.Segment(context.Background(), 0)
			if err != nil {
				t.Fatalf("second Segment() error = %v", err)
			}
			if !bytes.Equal(segment, []byte("segment-after-retry")) {
				t.Fatalf("second Segment() = %q", segment)
			}
			if pipeline.segmentCalls != 2 {
				t.Fatalf("pipeline Segment calls = %d, want 2", pipeline.segmentCalls)
			}
		})
	}
}

func TestTypedFailureCategoryTakesPrecedenceOverContextLikeCause(t *testing.T) {
	err := newFailure(FailureSourceIO, "vaapi", "renderD128", "prepare", context.DeadlineExceeded)
	if got := FailureCategoryOf(err); got != FailureSourceIO {
		t.Fatalf("FailureCategoryOf() = %q, want explicit %q", got, FailureSourceIO)
	}
}

func TestCPUAdmissionIsBoundedUntilRenditionRelease(t *testing.T) {
	contract := testOutputContract()
	prepareCalls := 0
	cpu := &fakeProvider{
		name:    "cpu",
		kind:    providerCPU,
		devices: []device{{ID: "cpu"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			prepareCalls++
			return &fakePipeline{output: pipelineOutput{
				Contract:             contract,
				Init:                 []byte("cpu-init"),
				ExtradataFingerprint: "sha256:cpu",
			}}, nil
		},
	}
	manager := newVideoRenditionManager(ManagerOptions{
		CPUConcurrency:   1,
		CPUAdmissionWait: 20 * time.Millisecond,
	}, cpu)
	request := RenditionRequest{Source: InputSpec{SourceID: "movie:0"}, Output: contract}

	first, err := manager.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}

	_, err = manager.Acquire(context.Background(), request)
	if FailureCategoryOf(err) != FailureAdmissionTimeout {
		t.Fatalf("second Acquire() category = %q, want %q (err=%v)", FailureCategoryOf(err), FailureAdmissionTimeout, err)
	}
	if prepareCalls != 1 {
		t.Fatalf("CPU Prepare called %d times while slot occupied, want once", prepareCalls)
	}

	first.Release()
	third, err := manager.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("Acquire() after Release error = %v", err)
	}
	third.Release()
	if prepareCalls != 2 {
		t.Fatalf("CPU Prepare calls after Release = %d, want 2", prepareCalls)
	}
}

func TestHardwareAdmissionIsScopedPerPhysicalDevice(t *testing.T) {
	contract := testOutputContract()
	hardware := &fakeProvider{
		name: "nvenc",
		kind: providerHardware,
		devices: []device{
			{ID: "gpu-fast", PerformanceScore: 100},
			{ID: "gpu-free", PerformanceScore: 50},
		},
		prepare: func(_ context.Context, d device, _ InputSpec, _ OutputContract, _ DecodeMode) (preparedPipeline, error) {
			return &fakePipeline{output: pipelineOutput{
				Contract:             contract,
				Init:                 []byte("init-" + d.ID),
				ExtradataFingerprint: "sha256:" + d.ID,
			}}, nil
		},
	}
	manager := newVideoRenditionManager(ManagerOptions{PerDeviceConcurrency: 1}, hardware)
	request := RenditionRequest{Source: InputSpec{SourceID: "movie:0"}, Output: contract}

	first, err := manager.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	defer first.Release()
	if got := first.Descriptor().DeviceID; got != "gpu-fast" {
		t.Fatalf("first device = %q, want gpu-fast", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	second, err := manager.Acquire(ctx, request)
	if err != nil {
		t.Fatalf("second Acquire() should use free device: %v", err)
	}
	defer second.Release()
	if got := second.Descriptor().DeviceID; got != "gpu-free" {
		t.Fatalf("second device = %q, want gpu-free", got)
	}
}

func TestBusyHardwareDoesNotSpillIntoCPUFallback(t *testing.T) {
	contract := testOutputContract()
	hardware := &fakeProvider{
		name:    "vaapi",
		kind:    providerHardware,
		devices: []device{{ID: "renderD128"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			return &fakePipeline{output: pipelineOutput{
				Contract:             contract,
				Init:                 []byte("hardware-init"),
				ExtradataFingerprint: "sha256:hardware",
			}}, nil
		},
	}
	cpu := &fakeProvider{
		name:    "cpu",
		kind:    providerCPU,
		devices: []device{{ID: "cpu"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			t.Fatal("temporary GPU contention spilled into CPU fallback")
			return nil, nil
		},
	}
	manager := newVideoRenditionManager(ManagerOptions{PerDeviceConcurrency: 1}, hardware, cpu)
	request := RenditionRequest{Source: InputSpec{SourceID: "movie:0"}, Output: contract}

	first, err := manager.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	defer first.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = manager.Acquire(ctx, request)
	if FailureCategoryOf(err) != FailureResourceExhausted {
		t.Fatalf("busy hardware category = %q, want %q (err=%v)", FailureCategoryOf(err), FailureResourceExhausted, err)
	}
}

func TestAcquireRejectsInvalidOutputContractBeforeProvider(t *testing.T) {
	provider := &fakeProvider{
		name:    "cpu",
		kind:    providerCPU,
		devices: []device{{ID: "cpu"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			t.Fatal("provider called for invalid output contract")
			return nil, nil
		},
	}
	manager := newVideoRenditionManager(ManagerOptions{}, provider)

	_, err := manager.Acquire(context.Background(), RenditionRequest{
		Source: InputSpec{SourceID: "movie:0", Width: 1920, Height: 1080},
		Output: OutputContract{Codec: CodecH264, Width: 1919, Height: 1080},
	})
	if FailureCategoryOf(err) != FailureInvalidOutput {
		t.Fatalf("Acquire() category = %q, want %q (err=%v)", FailureCategoryOf(err), FailureInvalidOutput, err)
	}
}

func TestSourceIOFailureDoesNotTriggerCPUFallback(t *testing.T) {
	contract := testOutputContract()
	hardware := &fakeProvider{
		name:    "vaapi",
		kind:    providerHardware,
		devices: []device{{ID: "renderD128"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			return nil, newFailure(FailureSourceIO, "vaapi", "renderD128", "prepare", errFakeTorrentRead)
		},
	}
	cpu := &fakeProvider{
		name:    "cpu",
		kind:    providerCPU,
		devices: []device{{ID: "cpu"}},
		prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
			t.Fatal("source I/O failure was incorrectly treated as a hardware failure")
			return nil, nil
		},
	}

	manager := newVideoRenditionManager(ManagerOptions{}, hardware, cpu)
	_, err := manager.Acquire(context.Background(), RenditionRequest{
		Source: InputSpec{SourceID: "movie:starved"},
		Output: contract,
	})
	if FailureCategoryOf(err) != FailureSourceIO || !errors.Is(err, errFakeTorrentRead) {
		t.Fatalf("Acquire() error = %v, want typed source I/O failure", err)
	}
}

func TestGenerationIDsDoNotRepeatAcrossManagerInstances(t *testing.T) {
	contract := testOutputContract()
	newProvider := func() videoProvider {
		return &fakeProvider{
			name:    "cpu",
			kind:    providerCPU,
			devices: []device{{ID: "cpu"}},
			prepare: func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error) {
				return &fakePipeline{output: pipelineOutput{
					Contract:             contract,
					Init:                 []byte("init"),
					ExtradataFingerprint: "sha256:cpu",
				}}, nil
			},
		}
	}
	request := RenditionRequest{Source: InputSpec{SourceID: "movie:0"}, Output: contract}

	first, err := newVideoRenditionManager(ManagerOptions{}, newProvider()).Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	defer first.Release()
	second, err := newVideoRenditionManager(ManagerOptions{}, newProvider()).Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	defer second.Release()

	if first.Descriptor().GenerationID == second.Descriptor().GenerationID {
		t.Fatalf("generation ID repeated across manager instances: %q", first.Descriptor().GenerationID)
	}
}

func testOutputContract() OutputContract {
	return OutputContract{
		Codec:       CodecH264,
		CodecString: "avc1.640029",
		Profile:     "high",
		Level:       "4.1",
		Width:       1920,
		Height:      1080,
		PixelFormat: "yuv420p",
		FrameRate:   Rational{Num: 24, Den: 1},
		TimeBase:    Rational{Num: 1, Den: 90000},
		RateControl: RateControl{TargetBitrate: 6_000_000, MaxBitrate: 8_000_000},
		GOP: GOPContract{
			SegmentDuration: Rational{Num: 6, Den: 1},
			Closed:          true,
			IDRAtBoundary:   true,
		},
		Color: ColorContract{
			Primaries: "bt709",
			Transfer:  "bt709",
			Matrix:    "bt709",
			Range:     "limited",
		},
	}
}

type fakeProvider struct {
	name     string
	kind     providerKind
	devices  []device
	prepare  func(context.Context, device, InputSpec, OutputContract, DecodeMode) (preparedPipeline, error)
	prepared []DecodeMode
}

func (p *fakeProvider) Name() string       { return p.name }
func (p *fakeProvider) Kind() providerKind { return p.kind }
func (p *fakeProvider) Inventory(context.Context) []device {
	return append([]device(nil), p.devices...)
}
func (p *fakeProvider) Prepare(ctx context.Context, d device, in InputSpec, out OutputContract, mode DecodeMode) (preparedPipeline, error) {
	p.prepared = append(p.prepared, mode)
	return p.prepare(ctx, d, in, out, mode)
}

type fakePipeline struct {
	output       pipelineOutput
	segments     map[int][]byte
	segment      func(context.Context, int) ([]byte, error)
	segmentCalls int
	closed       int
}

var errFakeUnsupported = errors.New("unsupported source profile")
var errFakeDeviceLost = errors.New("device lost")
var errFakeTorrentRead = errors.New("torrent bytes unavailable")

func slicesEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *fakePipeline) PrimedOutput() pipelineOutput { return p.output }
func (p *fakePipeline) Segment(ctx context.Context, index int) ([]byte, error) {
	p.segmentCalls++
	if p.segment != nil {
		return p.segment(ctx, index)
	}
	return append([]byte(nil), p.segments[index]...), nil
}
func (p *fakePipeline) Close() error {
	p.closed++
	return nil
}
