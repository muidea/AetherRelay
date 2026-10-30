// Package aetherrelaytrace contains bounded execution evidence. These values
// never participate in routing, retries, quota, or account health decisions.
package aetherrelaytrace

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"aetherrelay/internal/pkg/aetherrelaycodex"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
)

// BuildRevision is supplied by release/container builds with -ldflags.
// Empty means unavailable; runtime code must not substitute the current checkout.
var BuildRevision string

const MaxAttempts = 32
const MaxActions = 64
const MaxTraceBytes = 256 << 10

// Context is a request-start snapshot, not the current configuration on read.
// Explicit fields avoid accidentally retaining credentials or opaque sessions.
type Context struct {
	RequestID                string `json:"request_id,omitempty"`
	Revision                 string `json:"revision,omitempty"`
	Instance                 string `json:"instance,omitempty"`
	ArchiveEnabled           bool   `json:"archive_enabled"`
	ArchiveFullContent       bool   `json:"archive_full_content"`
	RequestTimeoutSeconds    int    `json:"request_timeout_seconds"`
	MaxSSELineBytes          int    `json:"max_sse_line_bytes"`
	WebsocketMaxMessageBytes int64  `json:"websocket_max_message_bytes"`
}

type Handshake struct {
	At              time.Time        `json:"at"`
	Status          int              `json:"status"`
	DurationMS      int64            `json:"duration_ms"`
	TransportReason transport.Reason `json:"transport_reason,omitempty"`
}

type Attempt struct {
	aetherrelaycodex.UpstreamError
	transport.WebsocketDiagnostics
	ID                   int              `json:"id"`
	Provider             string           `json:"provider"`
	AccountID            string           `json:"account_id,omitempty"`
	AccountLabel         string           `json:"account_label,omitempty"`
	Model                string           `json:"model,omitempty"`
	ResponseModel        string           `json:"response_model,omitempty"`
	StartedAt            time.Time        `json:"started_at"`
	ObservedAt           time.Time        `json:"observed_at"`
	Finished             bool             `json:"finished"`
	Failed               bool             `json:"failed"`
	Stage                string           `json:"stage"`
	Transport            string           `json:"transport,omitempty"`
	FallbackReason       string           `json:"fallback_reason,omitempty"`
	RequestSent          *bool            `json:"request_sent,omitempty"`
	UpstreamRequestID    string           `json:"upstream_request_id,omitempty"`
	UpstreamStatus       int              `json:"upstream_status,omitempty"`
	QueueWaitMS          int64            `json:"queue_wait_ms,omitempty"`
	HeadersDurationMS    int64            `json:"headers_duration_ms,omitempty"`
	ReadDurationMS       int64            `json:"read_duration_ms,omitempty"`
	FirstEventDurationMS int64            `json:"first_event_duration_ms,omitempty"`
	FirstOutputAt        *time.Time       `json:"first_output_at,omitempty"`
	LastEventAt          *time.Time       `json:"last_event_at,omitempty"`
	TerminalEvent        string           `json:"terminal_event,omitempty"`
	ReadObserved         bool             `json:"read_observed"`
	HeadersObserved      bool             `json:"headers_observed"`
	ErrorBodyFile        string           `json:"error_body_file,omitempty"`
	EventCount           int64            `json:"event_count"`
	WireBytes            int64            `json:"wire_bytes"`
	ErrorCode            string           `json:"error_code,omitempty"`
	FailureClass         string           `json:"failure_class,omitempty"`
	TransportReason      transport.Reason `json:"transport_reason,omitempty"`
	Handshakes           []Handshake      `json:"handshakes,omitempty"`
	ErrorBodyState       string           `json:"error_body_state"`
	ErrorFieldsTruncated bool             `json:"error_fields_truncated"`
}

type Action struct {
	At        time.Time `json:"at"`
	AttemptID int       `json:"attempt_id"`
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason,omitempty"`
}

type Trace struct {
	TotalAttempts          int       `json:"total_attempts"`
	ObservedFailures       int       `json:"observed_failures"`
	Version                int       `json:"version"`
	Context                Context   `json:"context"`
	Attempts               []Attempt `json:"attempts"`
	Actions                []Action  `json:"actions"`
	AttemptsTruncated      bool      `json:"attempts_truncated"`
	ActionsTruncated       bool      `json:"actions_truncated"`
	SaveFailed             bool      `json:"save_failed"`
	ReplaySuppressedReason string    `json:"replay_suppressed_reason,omitempty"`
}

// Summary can survive removal of detailed attempts and remains queryable
// without parsing JSON or reading interaction archives.
type Summary struct {
	TraceRequestID string `json:"trace_request_id,omitempty"`
	TraceRevision  string `json:"trace_revision,omitempty"`
	TraceState     string `json:"trace_state"`
	AttemptCount   int    `json:"attempt_count"`
	FailedAttempts int    `json:"failed_attempts"`
	HadAnomaly     bool   `json:"had_anomaly"`
	PrimaryCause   string `json:"primary_cause,omitempty"`
	TraceTransport string `json:"trace_transport,omitempty"`
}

func Cause(a Attempt) string {
	if a.UpstreamErrorCode == "server_is_overloaded" && a.FailureClass == "upstream_overloaded" {
		return "upstream_overloaded"
	}
	if a.UpstreamErrorCode != "" {
		return a.UpstreamErrorCode
	}
	if a.TransportReason != "" && a.TransportReason != transport.Unknown {
		return string(a.TransportReason)
	}
	if a.FailureClass != "" {
		return a.FailureClass
	}
	if a.ErrorCode != "" {
		return a.ErrorCode
	}
	return "unknown"
}

func (t Trace) Summary() Summary {
	s := Summary{TraceState: "saved", AttemptCount: len(t.Attempts), TraceRequestID: t.Context.RequestID, TraceRevision: t.Context.Revision}
	if t.AttemptsTruncated || t.ActionsTruncated {
		s.TraceState = "truncated"
	}

	for _, a := range t.Attempts {
		if a.Transport != "" {
			s.TraceTransport = a.Transport
		}
		if a.Failed {
			s.FailedAttempts++
			s.HadAnomaly = true
			s.PrimaryCause = Cause(a)
		}
		// A pre-send fallback is evidence of a recovered transport fault, even
		// when the same execution succeeds; it is not another generation attempt.
		if a.FallbackReason != "" {
			s.HadAnomaly = true
			if s.PrimaryCause == "" {
				s.PrimaryCause = a.FallbackReason
			}
		}
	}
	if t.TotalAttempts > s.AttemptCount {
		s.AttemptCount = t.TotalAttempts
	}
	if t.ObservedFailures > s.FailedAttempts {
		s.FailedAttempts = t.ObservedFailures
		s.HadAnomaly = true
	}
	return s
}

func Clone(t Trace) Trace {
	b, _ := json.Marshal(t)
	var out Trace
	_ = json.Unmarshal(b, &out)
	return out
}

// Bounded is a second boundary for callers outside the built-in collectors.
func Bounded(t Trace) Trace {
	t = Clone(t)
	t.Version = 1
	if len(t.Attempts) > t.TotalAttempts {
		t.TotalAttempts = len(t.Attempts)
	}
	failures := 0
	for _, a := range t.Attempts {
		if a.Failed {
			failures++
		}
	}
	if failures > t.ObservedFailures {
		t.ObservedFailures = failures
	}
	if len(t.Attempts) > MaxAttempts {
		t.Attempts = append(t.Attempts[:MaxAttempts-1], t.Attempts[len(t.Attempts)-1])
		t.AttemptsTruncated = true
	}
	if len(t.Actions) > MaxActions {
		t.Actions = t.Actions[:MaxActions]
		t.ActionsTruncated = true
	}
	t.Context.RequestID = short(t.Context.RequestID, 128)
	t.Context.Revision = short(t.Context.Revision, 128)
	t.Context.Instance = short(t.Context.Instance, 128)
	for i := range t.Attempts {
		a := &t.Attempts[i]
		original := a.UpstreamError
		a.UpstreamError = a.UpstreamError.Bounded()
		a.ErrorFieldsTruncated = a.ErrorFieldsTruncated || original != a.UpstreamError
		a.WebsocketDiagnostics = a.WebsocketDiagnostics.Safe()
		a.TransportReason = a.TransportReason.Safe()
		a.Provider = short(a.Provider, 256)
		a.AccountID = short(a.AccountID, 256)
		a.AccountLabel = short(a.AccountLabel, 320)
		a.Model = short(a.Model, 256)
		a.ResponseModel = short(a.ResponseModel, 256)
		a.ErrorCode = short(a.ErrorCode, 96)
		a.FailureClass = short(a.FailureClass, 96)
		a.UpstreamRequestID = short(a.UpstreamRequestID, 256)
		a.FallbackReason = short(a.FallbackReason, 96)
		a.Stage = short(a.Stage, 64)
		a.TerminalEvent = short(a.TerminalEvent, 96)
		a.ErrorBodyState = short(a.ErrorBodyState, 64)
		a.ErrorBodyFile = short(a.ErrorBodyFile, 96)
		if a.Transport != "http" && a.Transport != "websocket" {
			a.Transport = ""
		}
		if len(a.Handshakes) > 1 {
			a.Handshakes = a.Handshakes[:1]
			t.AttemptsTruncated = true
		}
		for j := range a.Handshakes {
			a.Handshakes[j].TransportReason = a.Handshakes[j].TransportReason.Safe()
		}
	}
	for i := range t.Actions {
		t.Actions[i].Kind = short(t.Actions[i].Kind, 64)
		t.Actions[i].Reason = short(t.Actions[i].Reason, 128)
	}
	t.ReplaySuppressedReason = short(t.ReplaySuppressedReason, 96)
	// JSON escaping can expand otherwise bounded UTF-8 strings. Retain the first
	// and final evidence while enforcing the encoded budget, including actions.
	for len(t.Attempts) > 2 {
		encoded, err := json.Marshal(t)
		if err != nil || len(encoded) <= MaxTraceBytes {
			break
		}
		last := t.Attempts[len(t.Attempts)-1]
		t.Attempts = append(t.Attempts[:len(t.Attempts)-2], last)
		t.AttemptsTruncated = true
	}
	return t
}
func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return aetherrelaycodex.ErrorText(s, n)
}

type Collector struct {
	mu               sync.Mutex
	trace            Trace
	nextID           int
	pendingHandshake *Handshake
}
type contextKey struct{}

func WithCollector(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, &Collector{trace: Trace{Version: 1, Attempts: []Attempt{}, Actions: []Action{}}})
}
func FromContext(ctx context.Context) *Collector {
	c, _ := ctx.Value(contextKey{}).(*Collector)
	return c
}
func (c *Collector) SetContext(v Context) { c.mu.Lock(); defer c.mu.Unlock(); c.trace.Context = v }
func (c *Collector) Snapshot() Trace      { c.mu.Lock(); defer c.mu.Unlock(); return Bounded(c.trace) }
func (c *Collector) MarkSaveFailed()      { c.mu.Lock(); defer c.mu.Unlock(); c.trace.SaveFailed = true }
func (c *Collector) Begin(provider, account, label, model string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	c.trace.TotalAttempts = c.nextID
	if len(c.trace.Attempts) >= MaxAttempts {
		c.trace.AttemptsTruncated = true
		c.trace.Attempts = c.trace.Attempts[:MaxAttempts-1]
	}
	if len(c.trace.Attempts) > 0 {
		previous := c.trace.Attempts[len(c.trace.Attempts)-1]
		kind := "same_account_reexecution"
		if previous.AccountID != account || previous.Provider != provider {
			kind = "account_or_provider_switch"
		}
		c.actionLocked(kind, "", previous.ID)
	}
	c.trace.Attempts = append(c.trace.Attempts, Attempt{ID: c.nextID, Provider: provider, AccountID: account, AccountLabel: label, Model: model, StartedAt: time.Now().UTC(), Stage: "dispatch", ErrorBodyState: "not_collected"})
	if c.pendingHandshake != nil {
		c.trace.Attempts[len(c.trace.Attempts)-1].Handshakes = []Handshake{*c.pendingHandshake}
		c.pendingHandshake = nil
	}
	return c.nextID
}
func (c *Collector) CurrentID() int { c.mu.Lock(); defer c.mu.Unlock(); return c.nextID }
func (c *Collector) Observe(a Attempt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.ID == 0 {
		a.ID = c.nextID
	}
	if a.ID == 0 {
		return
	}
	for i := range c.trace.Attempts {
		previous := c.trace.Attempts[i]
		if previous.ID != a.ID {
			continue
		}
		if (previous.Finished && !a.Finished) || (previous.Failed && !a.Failed) {
			return
		}
		if len(a.Handshakes) == 0 {
			a.Handshakes = previous.Handshakes
		}
		if a.ErrorBodyFile == "" && previous.ErrorBodyFile != "" {
			a.ErrorBodyFile = previous.ErrorBodyFile
			a.ErrorBodyState = previous.ErrorBodyState
		}
		a.StartedAt = previous.StartedAt
		a.Provider = previous.Provider
		a.AccountID = previous.AccountID
		a.AccountLabel = previous.AccountLabel
		if a.Model == "" {
			a.Model = previous.Model
		}
		if a.Failed && !previous.Failed {
			c.trace.ObservedFailures++
		}
		c.trace.Attempts[i] = a
		return
	}
}
func (c *Collector) Action(kind, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actionLocked(kind, reason, c.nextID)
}
func (c *Collector) actionLocked(kind, reason string, id int) {
	if len(c.trace.Actions) >= MaxActions {
		c.trace.ActionsTruncated = true
		return
	}
	c.trace.Actions = append(c.trace.Actions, Action{At: time.Now().UTC(), AttemptID: id, Kind: kind, Reason: reason})
}
func (c *Collector) SuppressReplay(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trace.ReplaySuppressedReason = reason
}

func (c *Collector) ArchiveBodyState(state, file string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.trace.Attempts) > 0 {
		c.trace.Attempts[len(c.trace.Attempts)-1].ErrorBodyState = state
		c.trace.Attempts[len(c.trace.Attempts)-1].ErrorBodyFile = file
	}
}

// RememberHandshake associates a native session opening with its next actual
// send. Opening a connection does not count as another generation execution.
func (c *Collector) RememberHandshake(h Handshake) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingHandshake = &h
}
