package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metrics "aetherrelay/internal/pkg/aetherrelaymetrics"
	metricsport "aetherrelay/internal/pkg/aetherrelaymetricsport"
)

// gatedStream sends the first event immediately, then holds the stream open
// until the test finishes it or cancels the request.
type gatedStream struct {
	first   *strings.Reader
	ctx     context.Context
	waiting chan<- struct{}
	release <-chan struct{}
	once    sync.Once
	last    *strings.Reader
}

func (s *gatedStream) Read(p []byte) (int, error) {
	if s.first.Len() > 0 {
		return s.first.Read(p)
	}
	s.once.Do(func() { s.waiting <- struct{}{} })
	select {
	case <-s.ctx.Done():
		return 0, s.ctx.Err()
	case <-s.release:
		return s.last.Read(p)
	}
}

func (s *gatedStream) Close() error { return nil }

func TestInflightCoversStreamsAndClientCancellation(t *testing.T) {
	h := testHandler("https://upstream.test", t.TempDir(), "openai")
	registry := metrics.NewRegistry()
	h.metricsRegistry = metricsport.Direct(registry)
	waiting := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	h.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: &gatedStream{
				first: strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"),
				ctx:   r.Context(), waiting: waiting, release: release,
				last: strings.NewReader("data: [DONE]\n\n"),
			},
		}, nil
	})
	assertInflight := func(want int64) {
		t.Helper()
		data, err := registry.StatsJSON()
		if err != nil {
			t.Fatal(err)
		}
		var stats metrics.StatsJSON
		if err := json.Unmarshal(data, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.Requests.Inflight != want {
			t.Fatalf("inflight = %d, want %d", stats.Requests.Inflight, want)
		}
	}
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("request did not reach expected state")
		}
	}
	done := make([]chan struct{}, 2)
	var cancel context.CancelFunc
	for i := range 2 {
		r := newRequest(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if i == 0 {
			ctx, stop := context.WithCancel(r.Context())
			cancel = stop
			defer stop()
			r = r.WithContext(ctx)
		}
		done[i] = make(chan struct{})
		go func() { defer close(done[i]); h.ServeHTTP(newResponseRecorder(), r) }()
	}
	wait(waiting)
	wait(waiting)
	assertInflight(2)
	cancel()
	wait(done[0])
	assertInflight(1)
	releaseOnce.Do(func() { close(release) })
	wait(done[1])
	assertInflight(0)
	for _, path := range []string{"/healthz", "/unknown", "/v1/chat/completions"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		h.ServeHTTP(newResponseRecorder(), r)
		assertInflight(0)
	}
}
