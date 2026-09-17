package media

import (
	"context"
	"fmt"
	"io"

	"github.com/asticode/go-astiav"
)

// avioBufferSize is the AVIOContext buffer libav reads through. 64 KiB matches libav's default probe
// granularity and keeps per-read syscalls into our cache coarse.
const avioBufferSize = 1 << 16

// openDemux opens `src` as a demuxable input entirely IN-PROCESS. A custom AVIOContext feeds libav bytes
// straight from our seekable reader (backed by the RAM torrent cache), so there is no file, no URL, no
// ffmpeg subprocess and no loopback HTTP. An IOInterrupter armed by `ctx` is the hard deadline: if the
// reader stalls (e.g. a non-resident tail read while libav parses the container index), libav's blocking
// I/O is interrupted and the call returns an error instead of hanging — this is the anti-hang guarantee,
// enforced by construction because WE own the I/O.
//
// On success it returns the opened, stream-info'd FormatContext and a cleanup func the caller MUST defer;
// on error everything is already torn down.
func openDemux(ctx context.Context, src io.ReadSeeker) (*astiav.FormatContext, func(), error) {
	// Custom AVIO over our reader: read + seek callbacks, no writer (input is read-only).
	ioCtx, err := astiav.AllocIOContext(
		avioBufferSize,
		false,
		func(b []byte) (int, error) { return src.Read(b) },
		func(offset int64, whence int) (int64, error) { return src.Seek(offset, whence) },
		nil,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("media: alloc io context: %w", err)
	}

	fc := astiav.AllocFormatContext()
	if fc == nil {
		ioCtx.Free()
		return nil, nil, fmt.Errorf("media: alloc format context")
	}
	fc.SetPb(ioCtx)

	// ctx-driven interrupt of libav's blocking I/O — the deadline backstop against a hang.
	interrupter := astiav.NewIOInterrupter()
	fc.SetIOInterrupter(interrupter)
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			interrupter.Interrupt()
		case <-stop:
		}
	}()

	cleanup := func() {
		close(stop)
		<-stopped // ensure the interrupt goroutine has exited before we free the interrupter
		fc.CloseInput()
		fc.Free()
		ioCtx.Free()
		interrupter.Free()
	}

	if err := fc.OpenInput("", nil, nil); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("media: open input: %w", err)
	}
	if err := fc.FindStreamInfo(nil); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("media: find stream info: %w", err)
	}
	return fc, cleanup, nil
}
