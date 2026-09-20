package rendition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

var errRenditionReleased = errors.New("rendition: released")

type videoRenditionManager struct {
	providers            []videoProvider
	deviceRank           map[string]int
	perDeviceConcurrency int
	cpuConcurrency       int
	cpuAdmissionWait     time.Duration
	admission            admissionRegistry
}

func newVideoRenditionManager(options ManagerOptions, providers ...videoProvider) VideoRenditionManager {
	ranks := make(map[string]int, len(options.DeviceOrder))
	for i, identity := range options.DeviceOrder {
		if _, exists := ranks[identity]; !exists {
			ranks[identity] = i
		}
	}
	if options.PerDeviceConcurrency <= 0 {
		options.PerDeviceConcurrency = 1
	}
	if options.CPUConcurrency <= 0 {
		options.CPUConcurrency = 1
	}
	if options.CPUAdmissionWait <= 0 {
		options.CPUAdmissionWait = 2 * time.Second
	}
	return &videoRenditionManager{
		providers:            append([]videoProvider(nil), providers...),
		deviceRank:           ranks,
		perDeviceConcurrency: options.PerDeviceConcurrency,
		cpuConcurrency:       options.CPUConcurrency,
		cpuAdmissionWait:     options.CPUAdmissionWait,
	}
}

func (m *videoRenditionManager) Acquire(ctx context.Context, request RenditionRequest) (VideoRendition, error) {
	if err := validateOutputContract(request.Output); err != nil {
		return nil, newFailure(FailureInvalidOutput, "", "", "validate", err)
	}
	var lastFailure error
	var hardwareContention error
	for _, candidate := range m.candidates(ctx) {
		if candidate.provider.Kind() == providerCPU && hardwareContention != nil {
			return nil, hardwareContention
		}
		lifecycle := newGenerationLifecycle()
		releaseAdmission, err := m.admit(ctx, candidate)
		if err != nil {
			lastFailure = err
			if candidate.provider.Kind() == providerHardware && FailureCategoryOf(err) == FailureResourceExhausted {
				hardwareContention = err
			}
			if canFallbackBeforeCommit(err) {
				continue
			}
			return nil, err
		}
		pipeline, err := candidate.provider.Prepare(ctx, candidate.device, request.Source, request.Output, candidate.mode)
		if err != nil {
			releaseAdmission()
			lastFailure = err
			if candidate.provider.Kind() == providerHardware && FailureCategoryOf(err) == FailureResourceExhausted {
				hardwareContention = err
			}
			if canFallbackBeforeCommit(err) {
				continue
			}
			return nil, err
		}
		if err := lifecycle.transition(generationCandidatePrepared); err != nil {
			_ = pipeline.Close()
			releaseAdmission()
			return nil, newFailure(FailureInvalidOutput, candidate.provider.Name(), candidate.device.ID, "prepare", err)
		}
		primed := pipeline.PrimedOutput()
		if primed.Contract != request.Output || len(primed.Init) == 0 || primed.ExtradataFingerprint == "" {
			_ = pipeline.Close()
			releaseAdmission()
			lastFailure = newFailure(FailureInvalidOutput, candidate.provider.Name(), candidate.device.ID, "prime", errors.New("provider returned incomplete or mismatched primed output"))
			continue
		}
		if err := lifecycle.transition(generationPrimed); err != nil {
			_ = pipeline.Close()
			releaseAdmission()
			return nil, newFailure(FailureInvalidOutput, candidate.provider.Name(), candidate.device.ID, "prime", err)
		}

		generationID, err := newGenerationID()
		if err != nil {
			_ = pipeline.Close()
			releaseAdmission()
			return nil, newFailure(FailureInvalidOutput, candidate.provider.Name(), candidate.device.ID, "commit", err)
		}
		if err := lifecycle.transition(generationCommitted); err != nil {
			_ = pipeline.Close()
			releaseAdmission()
			return nil, newFailure(FailureInvalidOutput, candidate.provider.Name(), candidate.device.ID, "commit", err)
		}
		return &managedRendition{
			descriptor: RenditionDescriptor{
				GenerationID:         generationID,
				Output:               primed.Contract,
				Bandwidth:            primed.Contract.RateControl.MaxBitrate * 11 / 10,
				Provider:             candidate.provider.Name(),
				DeviceID:             candidate.device.ID,
				ExtradataFingerprint: primed.ExtradataFingerprint,
			},
			init:             append([]byte(nil), primed.Init...),
			pipeline:         pipeline,
			lifecycle:        lifecycle,
			releaseAdmission: releaseAdmission,
		}, nil
	}
	if lastFailure != nil {
		return nil, lastFailure
	}
	return nil, newFailure(FailureDeviceUnavailable, "", "", "discover", errors.New("no video provider available"))
}

func newGenerationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "g-" + hex.EncodeToString(random[:]), nil
}

func validateOutputContract(contract OutputContract) error {
	if contract.Codec != CodecH264 || contract.CodecString == "" || contract.Profile != "high" || contract.Level == "" {
		return errors.New("H.264 High profile, level, and codec string are required")
	}
	if contract.Width <= 0 || contract.Height <= 0 || contract.Width%2 != 0 || contract.Height%2 != 0 {
		return errors.New("4:2:0 dimensions must be positive and even")
	}
	if contract.PixelFormat != "yuv420p" {
		return errors.New("8-bit yuv420p output is required")
	}
	if contract.FrameRate.Num <= 0 || contract.FrameRate.Den <= 0 || contract.TimeBase.Num <= 0 || contract.TimeBase.Den <= 0 {
		return errors.New("positive frame rate and time base are required")
	}
	if contract.RateControl.TargetBitrate <= 0 || contract.RateControl.MaxBitrate < contract.RateControl.TargetBitrate {
		return errors.New("valid constrained-VBR bitrate envelope is required")
	}
	if contract.GOP.SegmentDuration.Num <= 0 || contract.GOP.SegmentDuration.Den <= 0 || !contract.GOP.Closed || !contract.GOP.IDRAtBoundary {
		return errors.New("closed GOP with an IDR at every segment boundary is required")
	}
	if contract.Color.Primaries != "bt709" || contract.Color.Transfer != "bt709" || contract.Color.Matrix != "bt709" || contract.Color.Range == "" {
		return errors.New("explicit SDR BT.709 color contract is required")
	}
	return nil
}

func (m *videoRenditionManager) admit(ctx context.Context, candidate pipelineCandidate) (func(), error) {
	switch candidate.provider.Kind() {
	case providerCopy:
		return func() {}, nil
	case providerCPU:
		release, err := m.admission.gate("cpu", m.cpuConcurrency).acquire(ctx, m.cpuAdmissionWait)
		if err == nil {
			return release, nil
		}
		if errors.Is(err, errAdmissionWaitExpired) {
			return nil, newFailure(FailureAdmissionTimeout, candidate.provider.Name(), candidate.device.ID, "admission", err)
		}
		return nil, newFailure(FailureCancelled, candidate.provider.Name(), candidate.device.ID, "admission", err)
	case providerHardware:
		key := candidate.provider.Name() + "/" + candidate.device.ID
		release, err := m.admission.gate(key, m.perDeviceConcurrency).tryAcquire()
		if err == nil {
			return release, nil
		}
		return nil, newFailure(FailureResourceExhausted, candidate.provider.Name(), candidate.device.ID, "admission", err)
	default:
		return nil, newFailure(FailureDeviceUnavailable, candidate.provider.Name(), candidate.device.ID, "admission", errors.New("unknown provider kind"))
	}
}

type pipelineCandidate struct {
	provider videoProvider
	device   device
	mode     DecodeMode
}

func (m *videoRenditionManager) candidates(ctx context.Context) []pipelineCandidate {
	var copyCandidates, hardwareDecode, hardwareUpload, cpuCandidates []pipelineCandidate
	for _, provider := range m.providers {
		for _, d := range provider.Inventory(ctx) {
			switch provider.Kind() {
			case providerCopy:
				copyCandidates = append(copyCandidates, pipelineCandidate{provider: provider, device: d, mode: DecodeModeCopy})
			case providerHardware:
				if len(m.deviceRank) > 0 {
					if _, allowed := m.deviceRank[provider.Name()+"/"+d.ID]; !allowed {
						continue
					}
				}
				hardwareDecode = append(hardwareDecode, pipelineCandidate{provider: provider, device: d, mode: DecodeModeHardware})
				hardwareUpload = append(hardwareUpload, pipelineCandidate{provider: provider, device: d, mode: DecodeModeSoftware})
			case providerCPU:
				cpuCandidates = append(cpuCandidates, pipelineCandidate{provider: provider, device: d, mode: DecodeModeSoftware})
			}
		}
	}
	sortCandidates := func(candidates []pipelineCandidate) {
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].provider.Kind() == providerHardware {
				iKey := candidates[i].provider.Name() + "/" + candidates[i].device.ID
				jKey := candidates[j].provider.Name() + "/" + candidates[j].device.ID
				iUse := m.admission.gate(iKey, m.perDeviceConcurrency).inUse()
				jUse := m.admission.gate(jKey, m.perDeviceConcurrency).inUse()
				if iUse != jUse {
					return iUse < jUse
				}
			}
			if len(m.deviceRank) > 0 && candidates[i].provider.Kind() == providerHardware {
				iRank := m.deviceRank[candidates[i].provider.Name()+"/"+candidates[i].device.ID]
				jRank := m.deviceRank[candidates[j].provider.Name()+"/"+candidates[j].device.ID]
				return iRank < jRank
			}
			return candidates[i].device.PerformanceScore > candidates[j].device.PerformanceScore
		})
	}
	sortCandidates(copyCandidates)
	sortCandidates(hardwareDecode)
	sortCandidates(hardwareUpload)
	sortCandidates(cpuCandidates)

	ordered := make([]pipelineCandidate, 0, len(copyCandidates)+len(hardwareDecode)+len(hardwareUpload)+len(cpuCandidates))
	ordered = append(ordered, copyCandidates...)
	ordered = append(ordered, hardwareDecode...)
	ordered = append(ordered, hardwareUpload...)
	ordered = append(ordered, cpuCandidates...)
	return ordered
}

type managedRendition struct {
	mu               sync.Mutex
	pipelineMu       sync.Mutex
	descriptor       RenditionDescriptor
	init             []byte
	pipeline         preparedPipeline
	lifecycle        generationLifecycle
	terminalErr      error
	releaseOnce      sync.Once
	releaseAdmission func()
	released         bool
}

func (r *managedRendition) Descriptor() RenditionDescriptor { return r.descriptor }

func (r *managedRendition) Init(context.Context) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return nil, errRenditionReleased
	}
	return append([]byte(nil), r.init...), nil
}

func (r *managedRendition) Segment(ctx context.Context, index int) ([]byte, error) {
	r.pipelineMu.Lock()
	defer r.pipelineMu.Unlock()

	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return nil, errRenditionReleased
	}
	if r.lifecycle.state == generationFailed || r.lifecycle.state == generationComplete {
		err := r.terminalErr
		r.mu.Unlock()
		return nil, err
	}
	if r.lifecycle.state == generationCommitted {
		if err := r.lifecycle.transition(generationRunning); err != nil {
			r.mu.Unlock()
			return nil, withGeneration(err, r.descriptor.GenerationID)
		}
	}
	r.mu.Unlock()

	segment, err := r.pipeline.Segment(ctx, index)
	if err == nil {
		return append([]byte(nil), segment...), nil
	}
	err = withGeneration(err, r.descriptor.GenerationID)
	if FailureCategoryOf(err) == FailureCancelled {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.terminalErr = err
	if errors.Is(err, io.EOF) {
		_ = r.lifecycle.transition(generationComplete)
	} else {
		_ = r.lifecycle.transition(generationFailed)
	}
	return nil, err
}

func (r *managedRendition) Release() {
	r.releaseOnce.Do(func() {
		r.pipelineMu.Lock()
		defer r.pipelineMu.Unlock()
		r.mu.Lock()
		r.released = true
		r.mu.Unlock()
		_ = r.pipeline.Close()
		r.releaseAdmission()
	})
}
