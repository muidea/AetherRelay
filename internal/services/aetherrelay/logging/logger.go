// Package logging 配置 AetherRelay 进程的 slog 输出。
package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

func init() {
	slog.SetDefault(newLogger())
}

func ConfigureLogger(format string, debug bool) {
	slog.SetDefault(newLoggerWithOptions(os.Stderr, format, debug))
}

// newLogger 根据环境变量构造 AetherRelay 使用的 slog logger。
// LOG_FORMAT=text 时输出人类可读 key=value,否则默认 JSON。
// AETHERRELAY_VERBOSE_LOGGING=true 启用 Debug 级别,否则 Info 起跳。
func newLogger() *slog.Logger {
	return newLoggerWithOptions(os.Stderr, os.Getenv("LOG_FORMAT"), parseDebugFlag())
}

func newLoggerWithWriter(w io.Writer) *slog.Logger {
	return newLoggerWithOptions(w, os.Getenv("LOG_FORMAT"), parseDebugFlag())
}

func newLoggerWithOptions(w io.Writer, format string, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(newLevelColorWriter(w), opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&requestLogHandler{handler: handler})
}

// magicEngine emits access traces and route registration at INFO. Apply the
// application's verbosity policy without changing the vendored framework.
type requestLogHandler struct {
	handler slog.Handler
}

func (h *requestLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *requestLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level == slog.LevelInfo {
		switch record.Message {
		case "request started", "addRoute":
			record.Level = slog.LevelDebug
		case "request completed":
			record.Level = slog.LevelDebug
			record.Attrs(func(attr slog.Attr) bool {
				if attr.Key == "status" && attr.Value.Kind() == slog.KindInt64 {
					if attr.Value.Int64() >= 500 {
						record.Level = slog.LevelError
					} else if attr.Value.Int64() >= 400 {
						record.Level = slog.LevelWarn
					}
					return false
				}
				return true
			})
		}
	}
	if !h.handler.Enabled(ctx, record.Level) {
		return nil
	}
	return h.handler.Handle(ctx, record)
}

func (h *requestLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &requestLogHandler{handler: h.handler.WithAttrs(attrs)}
}

func (h *requestLogHandler) WithGroup(name string) slog.Handler {
	return &requestLogHandler{handler: h.handler.WithGroup(name)}
}

func parseDebugFlag() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AETHERRELAY_VERBOSE_LOGGING")))
	return v == "1" || v == "true" || v == "yes"
}

type levelColorWriter struct {
	out io.Writer
	mu  sync.Mutex
	buf []byte
}

func newLevelColorWriter(out io.Writer) io.Writer {
	return &levelColorWriter{out: out}
}

func (w *levelColorWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			return len(p), nil
		}
		line := append([]byte(nil), w.buf[:idx+1]...)
		w.buf = w.buf[idx+1:]
		if _, err := w.out.Write(colorizeLogLine(line)); err != nil {
			return 0, err
		}
	}
}

func colorizeLogLine(line []byte) []byte {
	token, color := logLevelToken(line)
	if token == "" || color == "" {
		return line
	}

	before, after, found := bytes.Cut(line, []byte(token))
	if !found {
		return line
	}

	// 只给 level=XXX 着色,避免整行颜色干扰阅读。
	colored := make([]byte, 0, len(line)+len(color)+len(ansiReset))
	colored = append(colored, before...)
	colored = append(colored, color...)
	colored = append(colored, token...)
	colored = append(colored, ansiReset...)
	colored = append(colored, after...)
	return colored
}

func logLevelToken(line []byte) (token, color string) {
	switch {
	case bytes.Contains(line, []byte("level=DEBUG")):
		return "level=DEBUG", ansiCyan
	case bytes.Contains(line, []byte("level=INFO")):
		return "level=INFO", ansiGreen
	case bytes.Contains(line, []byte("level=WARN")):
		return "level=WARN", ansiYellow
	case bytes.Contains(line, []byte("level=ERROR")):
		return "level=ERROR", ansiRed
	default:
		return "", ""
	}
}

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)
