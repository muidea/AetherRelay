package config

import (
	"testing"
	"time"
)

func TestCodexStreamMaximumDurationConfig(t *testing.T) {
	// CP-STREAM-013: opt-in maximum, independent of unary request timeout.
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		invalid bool
	}{
		{"0", 0, false}, {"900", 15 * time.Minute, false}, {"-1", 0, true}, {"oops", 0, true}, {"9223372036854775807", 0, true},
	} {
		cfg := Config{RequestTimeout: 5 * time.Minute}
		err := setCodexOAuth(&cfg, "stream_max_duration_seconds", tc.raw)
		if (err != nil) != tc.invalid || (!tc.invalid && cfg.CodexOAuth.StreamMaxDuration != tc.want) {
			t.Fatalf("value=%s cfg=%+v err=%v", tc.raw, cfg.CodexOAuth, err)
		}
		if cfg.RequestTimeout != 5*time.Minute {
			t.Fatal("modified unary timeout")
		}
	}
}
