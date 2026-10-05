package media

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/asticode/go-astiav"
)

// InitLogging clamps libav's av_log to errors and bridges it into slog. Without this, av_log writes
// straight to stderr at its default (info) level, flooding the container log with per-demux probe noise
// ("Could not find codec parameters ... (Attachment: none)", "Consider increasing ... 'analyzeduration'",
// "[swscaler] deprecated pixel format used") on every thumbnail/probe/segment. The callback therefore
// only ever receives error-severity messages; anything less severe is dropped by libav itself.
func InitLogging() {
	astiav.SetLogLevel(astiav.LogLevelError)
	astiav.SetLogCallback(func(c astiav.Classer, _ astiav.LogLevel, _ string, msg string) {
		msg = strings.TrimSpace(msg)
		if msg == "" {
			return
		}
		slog.Error("libav", "libav_class", libavClassName(c), "msg", msg)
	})
}

func libavClassName(c astiav.Classer) string {
	if c == nil {
		return ""
	}
	cls := c.Class()
	if cls == nil {
		return ""
	}
	return cls.Name()
}

var transcoderChoiceLogged sync.Map // pipeline key → already logged at info

// logTranscoderChoiceOnce logs a transcoder's codec/device selection at info the FIRST time a unique
// pipeline (key = kind|decoder|hardware|encoder) opens, and at debug afterwards. Transcoders are opened
// per segment, so without the dedup a transcode-path file would repeat the line ~10x/minute; the first
// line is what confirms the correct encode device was picked.
func logTranscoderChoiceOnce(msg, key string, attrs ...any) {
	if _, loaded := transcoderChoiceLogged.LoadOrStore(key, true); loaded {
		slog.Debug(msg, attrs...)
		return
	}
	slog.Info(msg, attrs...)
}
