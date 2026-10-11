package metrics

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestInflightConcurrentRequests(t *testing.T) {
	r := NewRegistry()
	assertInflight := func(want int64) {
		t.Helper()
		data, err := r.StatsJSON()
		if err != nil {
			t.Fatal(err)
		}
		var stats StatsJSON
		if err := json.Unmarshal(data, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.Requests.Inflight != want || stats.Requests.Total != 0 {
			t.Fatalf("requests = %+v, want inflight %d and total 0", stats.Requests, want)
		}
		var out strings.Builder
		if err := r.WritePrometheus(&out); err != nil {
			t.Fatal(err)
		}
		mustContain(t, out.String(), "# TYPE aetherrelay_requests_inflight gauge")
	}
	assertInflight(0)
	const count = 32
	started := make(chan struct{}, count)
	release := make(chan struct{})
	var finished sync.WaitGroup
	for range count {
		finished.Add(1)
		go func() {
			defer finished.Done()
			if err := r.BeginRequest(); err != nil {
				t.Error(err)
			}
			started <- struct{}{}
			<-release
			if err := r.EndRequest(); err != nil {
				t.Error(err)
			}
		}()
	}
	for range count {
		<-started
	}
	assertInflight(count)
	var out strings.Builder
	if err := r.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.String(), "aetherrelay_requests_inflight 32\n")
	close(release)
	finished.Wait()
	assertInflight(0)
	if err := r.EndRequest(); err == nil {
		t.Fatal("unmatched finish must fail")
	}
	assertInflight(0)
}
