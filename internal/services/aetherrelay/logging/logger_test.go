package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestFrameworkRequestLogsFollowApplicationVerbosity(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, debug := range []bool{false, true} {
			var out bytes.Buffer
			logger := newLoggerWithOptions(&out, format, debug).With("component", "http").WithGroup("request")
			logger.Info("request started", "serial", 1, "path", "/admin/api/usage/runtime")
			logger.Info("request completed", "serial", 1, "status", 200)
			logger.Info("addRoute", "pattern", "/healthz")
			if !debug && out.Len() != 0 {
				t.Fatalf("%s: routine request logs at INFO: %s", format, out.String())
			}
			if debug && strings.Count(out.String(), "\n") != 3 {
				t.Fatalf("%s: missing DEBUG access traces: %s", format, out.String())
			}
			if debug && !strings.Contains(out.String(), "DEBUG") {
				t.Fatalf("%s: traces must be DEBUG: %s", format, out.String())
			}
			out.Reset()
			logger.Info("request completed", "serial", 2, "status", 401)
			logger.Info("request completed", "serial", 3, "status", 502)
			logger.Info("server listening", "addr", "127.0.0.1:8080")
			logger.Warn("slow request", "status", 200)
			if strings.Count(out.String(), "\n") != 4 || !strings.Contains(out.String(), "WARN") || !strings.Contains(out.String(), "ERROR") || !strings.Contains(out.String(), "INFO") {
				t.Fatalf("%s: missing failure, startup or slow-request logs: %s", format, out.String())
			}
			if format == "json" {
				var record map[string]any
				if err := json.Unmarshal(bytes.Split(out.Bytes(), []byte("\n"))[0], &record); err != nil {
					t.Fatal(err)
				}
				if record["component"] != "http" || record["request"].(map[string]any)["status"] != float64(401) {
					t.Fatalf("lost logger attributes or groups: %v", record)
				}
			}
		}
	}
}

func TestTextLoggerColorsLevelTokenOnly(t *testing.T) {
	t.Setenv("LOG_FORMAT", "text")

	var out bytes.Buffer
	logger := newLoggerWithWriter(&out)

	logger.WarnContext(context.Background(), "warn message")
	logger.ErrorContext(context.Background(), "error message")

	logs := out.String()
	if !strings.Contains(logs, ansiYellow+"level=WARN"+ansiReset) {
		t.Fatalf("expected only WARN level token to be yellow, got: %q", logs)
	}
	if !strings.Contains(logs, ansiRed+"level=ERROR"+ansiReset) {
		t.Fatalf("expected only ERROR level token to be red, got: %q", logs)
	}
	if strings.Contains(logs, ansiYellow+"time=") || strings.Contains(logs, ansiRed+"time=") {
		t.Fatalf("expected timestamp to remain uncolored, got: %q", logs)
	}
	if got := strings.Count(logs, ansiReset); got != 2 {
		t.Fatalf("expected one reset per colored level token, got %d in %q", got, logs)
	}
}

func TestJSONLoggerDoesNotColorizeStructuredOutput(t *testing.T) {
	t.Setenv("LOG_FORMAT", "json")

	var out bytes.Buffer
	logger := newLoggerWithWriter(&out)

	logger.ErrorContext(context.Background(), "error message", slog.String("provider", "openai"))

	logs := out.String()
	if strings.Contains(logs, "\033[") {
		t.Fatalf("expected JSON logs without ANSI color codes, got: %q", logs)
	}
	if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, `"provider":"openai"`) {
		t.Fatalf("expected structured ERROR log, got: %q", logs)
	}
}
