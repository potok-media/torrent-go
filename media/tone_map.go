package media

import (
	"errors"
	"fmt"

	"github.com/asticode/go-astiav"
)

func isHDRTransfer(transfer astiav.ColorTransferCharacteristic) bool {
	return transfer == astiav.ColorTransferCharacteristicSmpte2084 ||
		transfer == astiav.ColorTransferCharacteristicAribStdB67
}

func vaapiToneMapFilter() string {
	// tonemap_vaapi defaults to an HDR-to-SDR conversion when no output mastering display is supplied.
	return "tonemap_vaapi=format=nv12:p=bt709:t=bt709:m=bt709"
}

func softwareToneMapFilter(output astiav.PixelFormat) string {
	// Convert PQ/HLG to linear float, apply a filmic curve, then return an explicitly tagged limited-range
	// BT.709 8-bit format. The software route is deliberately the portable last fallback; VAAPI HDR10 uses
	// the device VPP path instead.
	return "zscale=t=linear:npl=100," +
		"format=gbrpf32le," +
		"zscale=p=bt709," +
		"tonemap=tonemap=hable:desat=0," +
		"zscale=t=bt709:m=bt709:r=tv," +
		"format=pix_fmts=" + output.Name()
}

type videoToneMapper struct {
	graph *astiav.FilterGraph
	src   *astiav.BuffersrcFilterContext
	sink  *astiav.BuffersinkFilterContext
	frame *astiav.Frame
}

func newVideoToneMapper(input *astiav.Frame, timeBase astiav.Rational, content string, hwDev *astiav.HardwareDeviceContext, hardwareInput bool) (*videoToneMapper, error) {
	graph := astiav.AllocFilterGraph()
	if graph == nil {
		return nil, errors.New("media: allocate tone-map filter graph")
	}
	graph.SetThreadCount(cpuEncoderThreadLimit())
	fail := func(err error) (*videoToneMapper, error) {
		graph.Free()
		return nil, err
	}

	buffer := astiav.FindFilterByName("buffer")
	buffersink := astiav.FindFilterByName("buffersink")
	if buffer == nil || buffersink == nil {
		return fail(errors.New("media: video buffer filters unavailable"))
	}
	src, err := graph.NewBuffersrcFilterContext(buffer, "in")
	if err != nil {
		return fail(fmt.Errorf("media: create tone-map source: %w", err))
	}
	sink, err := graph.NewBuffersinkFilterContext(buffersink, "out")
	if err != nil {
		return fail(fmt.Errorf("media: create tone-map sink: %w", err))
	}

	params := astiav.AllocBuffersrcFilterContextParameters()
	if params == nil {
		return fail(errors.New("media: allocate tone-map source parameters"))
	}
	params.SetWidth(input.Width())
	params.SetHeight(input.Height())
	params.SetPixelFormat(input.PixelFormat())
	params.SetSampleAspectRatio(input.SampleAspectRatio())
	params.SetTimeBase(timeBase)
	params.SetColorRange(input.ColorRange())
	params.SetColorSpace(input.ColorSpace())
	if hardwareInput {
		params.SetHardwareFramesContext(input.HardwareFramesContext())
	}
	err = src.SetParameters(params)
	params.Free()
	if err != nil {
		return fail(fmt.Errorf("media: configure tone-map source: %w", err))
	}
	if err := src.Initialize(nil); err != nil {
		return fail(fmt.Errorf("media: initialize tone-map source: %w", err))
	}

	outputs := astiav.AllocFilterInOut()
	inputs := astiav.AllocFilterInOut()
	if outputs == nil || inputs == nil {
		if outputs != nil {
			outputs.Free()
		}
		if inputs != nil {
			inputs.Free()
		}
		return fail(errors.New("media: allocate tone-map filter links"))
	}
	defer outputs.Free()
	defer inputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(src.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)
	inputs.SetName("out")
	inputs.SetFilterContext(sink.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	if err := graph.Parse(content, inputs, outputs); err != nil {
		return fail(fmt.Errorf("media: parse tone-map filter %q: %w", content, err))
	}
	if hwDev != nil {
		for _, filter := range graph.Filters() {
			filter.SetHardwareDeviceContext(hwDev)
		}
	}
	if err := graph.Configure(); err != nil {
		return fail(fmt.Errorf("media: configure tone-map graph: %w", err))
	}
	frame := astiav.AllocFrame()
	if frame == nil {
		return fail(errors.New("media: allocate tone-map output frame"))
	}
	return &videoToneMapper{graph: graph, src: src, sink: sink, frame: frame}, nil
}

func (m *videoToneMapper) process(input *astiav.Frame, consume func(*astiav.Frame) error) error {
	if err := m.src.AddFrame(input, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
		return fmt.Errorf("media: submit frame to tone mapper: %w", err)
	}
	return m.drain(consume)
}

func (m *videoToneMapper) flush(consume func(*astiav.Frame) error) error {
	if err := m.src.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil && !errors.Is(err, astiav.ErrEof) {
		return fmt.Errorf("media: flush tone mapper: %w", err)
	}
	return m.drain(consume)
}

func (m *videoToneMapper) drain(consume func(*astiav.Frame) error) error {
	for {
		err := m.sink.GetFrame(m.frame, astiav.NewBuffersinkFlags())
		if errors.Is(err, astiav.ErrEagain) || errors.Is(err, astiav.ErrEof) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("media: receive tone-mapped frame: %w", err)
		}
		if err := consume(m.frame); err != nil {
			m.frame.Unref()
			return err
		}
		m.frame.Unref()
	}
}

func (m *videoToneMapper) free() {
	m.frame.Free()
	m.graph.Free()
}
