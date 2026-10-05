package main

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// ANSI colors (standard palette — deliberately NOT the bright variants).
const (
	ansiReset  = "\x1b[0m"
	ansiGray   = "\x1b[90m" // debug
	ansiGreen  = "\x1b[32m" // info
	ansiYellow = "\x1b[33m" // warn
	ansiRed    = "\x1b[31m" // error
)

// colorTextHandler renders the same `time=… level=… msg=… k=v` text format as slog.TextHandler, but
// with the level and message ANSI-colored by severity so container logs are scannable at a glance.
type colorTextHandler struct {
	out   io.Writer
	mu    *sync.Mutex
	level slog.Level
	attrs []slog.Attr
}

func newColorTextHandler(out io.Writer, level slog.Level) slog.Handler {
	return &colorTextHandler{out: out, mu: &sync.Mutex{}, level: level}
}

func (h *colorTextHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *colorTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

// WithGroup is a pass-through: the codebase logs flat key=value pairs only.
func (h *colorTextHandler) WithGroup(string) slog.Handler { return h }

func (h *colorTextHandler) Handle(_ context.Context, r slog.Record) error {
	color := colorForLevel(r.Level)
	var b strings.Builder
	b.WriteString("time=")
	b.WriteString(r.Time.Format("2006-01-02T15:04:05.000Z07:00"))
	b.WriteString(" level=")
	b.WriteString(color)
	b.WriteString(r.Level.String())
	b.WriteString(ansiReset)
	b.WriteString(" msg=")
	b.WriteString(color)
	b.WriteString(quoteIfNeeded(r.Message))
	b.WriteString(ansiReset)
	for _, a := range h.attrs {
		writeLogAttr(&b, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeLogAttr(&b, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func colorForLevel(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return ansiRed
	case l >= slog.LevelWarn:
		return ansiYellow
	case l >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiGray
	}
}

func writeLogAttr(b *strings.Builder, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	b.WriteByte(' ')
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(quoteIfNeeded(a.Value.String()))
}

func quoteIfNeeded(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"=") {
		return strconv.Quote(s)
	}
	return s
}
