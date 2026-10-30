package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestUsageTraceAndAnomalyEndpoints(t *testing.T) {
	ctx := context.Background()
	store := usage.NewMemoryStore()
	at := time.Now().UTC()
	if err := store.Start(ctx, usage.StartRecord{EventID: "trace-event", APIKeyID: "key", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	tr := trace.Trace{Context: trace.Context{RequestID: "server-request", Revision: "revision", Instance: "instance"}, Attempts: []trace.Attempt{{ID: 1, Finished: true, Failed: true, Transport: "websocket", FailureClass: "upstream_overloaded", ErrorBodyState: "not_collected"}}}
	if err := store.Complete(ctx, usage.CompleteRecord{EventID: "trace-event", HTTPStatus: 200, Outcome: "upstream_truncated", Trace: &tr}); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithUsage("", &fakeRuntime{cfg: config.Config{}}, store)
	for _, tc := range []struct {
		url, remote string
		status      int
	}{{"/admin/api/usage/trace?event_id=trace-event", "127.0.0.1:1", 200}, {"/admin/api/usage/trace?event_id=trace-event&download=1", "127.0.0.1:1", 200}, {"/admin/api/usage/trace?event_id=missing", "127.0.0.1:1", 404}, {"/admin/api/usage/trace", "127.0.0.1:1", 400}, {"/admin/api/usage/anomalies?range=all&timezone=Asia%2FSingapore", "127.0.0.1:1", 200}, {"/admin/api/usage/anomalies?group_by=raw_sql", "127.0.0.1:1", 400}, {"/admin/api/usage/events?result=invalid", "127.0.0.1:1", 400}, {"/admin/api/usage/events?timezone=arbitrary", "127.0.0.1:1", 400}, {"/admin/api/usage/events.csv?range=all", "127.0.0.1:1", 200}, {"/admin/api/usage/trace?event_id=trace-event", "8.8.8.8:10", 403}, {"/admin/api/usage/anomalies", "8.8.8.8:10", 403}, {"/admin/api/usage/events.csv", "8.8.8.8:10", 403}} {
		t.Run(tc.url+tc.remote, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			r.RemoteAddr = tc.remote
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			if tc.status == 200 && strings.Contains(tc.url, "/trace?") {
				var d usage.TraceDetail
				if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
					t.Fatal(err)
				}
				if d.Trace == nil || d.Trace.Context.RequestID != "server-request" || d.Event.Outcome != "upstream_truncated" || len(d.Timeline) == 0 {
					t.Fatal(d)
				}
				if strings.Contains(tc.url, "download=1") && !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
					t.Fatal(w.Header())
				}
			}
			if tc.status == 200 && strings.Contains(tc.url, "events.csv") {
				if !strings.Contains(w.Header().Get("Content-Type"), "text/csv") || !strings.Contains(w.Body.String(), "attempt_count") {
					t.Fatal(w.Header(), w.Body.String())
				}
			}
		})
	}
}
func TestUsageTraceDoesNotInventHistoricalDetails(t *testing.T) {
	s := usage.NewMemoryStore()
	_ = s.Start(context.Background(), usage.StartRecord{EventID: "legacy", APIKeyID: "key"})
	_ = s.Complete(context.Background(), usage.CompleteRecord{EventID: "legacy", HTTPStatus: 200, Outcome: "success"})
	h := NewHandlerWithUsage("", &fakeRuntime{cfg: config.Config{}}, s)
	r := httptest.NewRequest("GET", "/admin/api/usage/trace?event_id=legacy", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var d usage.TraceDetail
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d.Trace != nil || d.Evidence[0].State != "not_collected" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestUsageSingaporeRangeBoundaries(t *testing.T) {
	r := httptest.NewRequest("GET", "/admin/api/usage/events?timezone=Asia%2FSingapore&range=7d", nil)
	f, err := parseUsageFilter(r)
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("Asia/Singapore", 8*3600)
	if f.From.In(zone).Hour() != 0 || f.To.Sub(f.From) != 7*24*time.Hour {
		t.Fatalf("range %+v", f)
	}
	r = httptest.NewRequest("GET", "/admin/api/usage/events?timezone=Asia%2FSingapore&from=2026-10-30T00:00:00%2B08:00&to=2026-10-31T00:00:00%2B08:00", nil)
	f, err = parseUsageFilter(r)
	if err != nil || f.From.Format(time.RFC3339) != "2026-10-29T16:00:00Z" {
		t.Fatal(f, err)
	}
}

type cappedUsageExportStore struct {
	usage.Store
	count    int64
	called   bool
	oversize bool
}

func (s *cappedUsageExportStore) Count(context.Context, usage.UsageFilter) (int64, error) {
	return s.count, nil
}
func (s *cappedUsageExportStore) ExportCSV(_ context.Context, _ usage.UsageFilter, w io.Writer) error {
	s.called = true
	if s.oversize {
		_, err := w.Write(make([]byte, usageCSVMaxBytes+1))
		return err
	}
	_, err := w.Write([]byte("csv"))
	return err
}
func TestUsageTraceExportLimitsDoNotSendPartialSuccess(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		s := &cappedUsageExportStore{Store: usage.NewMemoryStore(), count: 10001}
		want := 400
		if oversize {
			s.count = 1
			s.oversize = true
			want = 503
		}
		h := NewHandlerWithUsage("", &fakeRuntime{cfg: config.Config{}}, s)
		r := httptest.NewRequest("GET", "/admin/api/usage/events.csv?range=all", nil)
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || s.called != oversize || strings.Contains(w.Header().Get("Content-Type"), "text/csv") || w.Header().Get("Content-Disposition") != "" {
			t.Fatalf("status=%d called=%v headers=%v", w.Code, s.called, w.Header())
		}
	}
}
