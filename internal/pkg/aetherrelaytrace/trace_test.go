package aetherrelaytrace

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"aetherrelay/internal/pkg/aetherrelaycodex"
)

func TestCollectorKeepsExecutionIdentityAndRecoveredFailure(t *testing.T) {
	c := FromContext(WithCollector(context.Background()))
	c.SetContext(Context{RequestID: "request", Instance: "instance"})
	id := c.Begin("codexoauth", "slot", "operator@example.com", "model")
	c.Observe(Attempt{Transport: "websocket", Handshakes: []Handshake{{Status: 101, DurationMS: 215}}})
	c.Observe(Attempt{Finished: true, Failed: true, Transport: "websocket", FailureClass: "upstream_overloaded", UpstreamError: aetherrelaycodex.UpstreamError{UpstreamErrorCode: "server_is_overloaded"}})
	c.Observe(Attempt{Transport: "websocket"}) // a late header cannot erase a terminal failure
	c.Action("credential_refresh", "invalid_token")
	id2 := c.Begin("codexoauth", "slot", "operator@example.com", "model")
	c.Observe(Attempt{Finished: true, Transport: "http"})
	got := c.Snapshot()
	s := got.Summary()
	if id != 1 || id2 != 2 || len(got.Attempts) != 2 || s.AttemptCount != 2 || s.FailedAttempts != 1 || !s.HadAnomaly || s.PrimaryCause != "upstream_overloaded" || got.Attempts[0].AccountLabel != "operator@example.com" {
		t.Fatalf("trace=%+v summary=%+v", got, s)
	}
	if !got.Attempts[0].Failed || !got.Attempts[1].Finished || len(got.Actions) != 2 {
		t.Fatalf("lost evidence: %+v", got)
	}
	got.Attempts[0].Failed = false
	if !c.Snapshot().Attempts[0].Failed {
		t.Fatal("snapshot aliases collector")
	}
}
func TestCollectorBoundsAndKeepsLastTerminalEvidence(t *testing.T) {
	c := FromContext(WithCollector(context.Background()))
	for i := 0; i < MaxAttempts+4; i++ {
		c.Begin("codexoauth", "slot", "label", "model")
		c.Observe(Attempt{Finished: true, Failed: true, UpstreamError: aetherrelaycodex.UpstreamError{UpstreamErrorMessage: strings.Repeat("错误", 2000)}})
		c.Action("credential_refresh", strings.Repeat("x", 1000))
	}
	s := c.Snapshot()
	if !s.AttemptsTruncated || len(s.Attempts) != MaxAttempts || s.Attempts[len(s.Attempts)-1].ID != MaxAttempts+4 || s.Summary().FailedAttempts != MaxAttempts+4 || !s.ActionsTruncated {
		t.Fatalf("incorrect bounded trace %+v", s)
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > MaxTraceBytes {
		t.Fatalf("payload %d err %v", len(b), err)
	}
}
func TestCollectorConcurrentObservationAndClonePointers(t *testing.T) {
	c := FromContext(WithCollector(context.Background()))
	c.Begin("codexoauth", "a", "label", "model")
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				at := time.Now()
				sent := true
				c.Observe(Attempt{FirstOutputAt: &at, RequestSent: &sent})
				_ = c.Snapshot()
			}
		}()
	}
	wg.Wait()
	t1 := c.Snapshot()
	*t1.Attempts[0].RequestSent = false
	if !*c.Snapshot().Attempts[0].RequestSent {
		t.Fatal("pointer leaked")
	}
}

func TestBoundedEnforcesEncodedJSONBudget(t *testing.T) {
	tr := Trace{}
	for i := 0; i < MaxAttempts; i++ {
		tr.Attempts = append(tr.Attempts, Attempt{ID: i + 1, Failed: i == MaxAttempts-1, Provider: strings.Repeat("<", 256), AccountID: strings.Repeat("<", 256), AccountLabel: strings.Repeat("<", 320), Model: strings.Repeat("<", 256), ResponseModel: strings.Repeat("<", 256), UpstreamRequestID: strings.Repeat("<", 256), UpstreamError: aetherrelaycodex.UpstreamError{UpstreamErrorMessage: strings.Repeat("<", 512)}})
	}
	for i := 0; i < MaxActions; i++ {
		tr.Actions = append(tr.Actions, Action{Kind: strings.Repeat("<", 64), Reason: strings.Repeat("<", 128)})
	}
	bounded := Bounded(tr)
	encoded, err := json.Marshal(bounded)
	if err != nil || len(encoded) > MaxTraceBytes || !bounded.AttemptsTruncated || bounded.TotalAttempts != MaxAttempts || bounded.Attempts[len(bounded.Attempts)-1].ID != MaxAttempts || bounded.Summary().FailedAttempts != 1 {
		t.Fatalf("bytes=%d summary=%+v err=%v", len(encoded), bounded.Summary(), err)
	}
}
