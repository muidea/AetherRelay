package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	"aetherrelay/internal/pkg/aetherrelaycodex"
	trace "aetherrelay/internal/pkg/aetherrelaytrace"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

var executionInstance = func() string { host, _ := os.Hostname(); return host + "/" + newRequestID() }()
var executionRevision = func() string {
	if trace.BuildRevision != "" {
		return trace.BuildRevision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}()

func (h *Handler) startExecutionTrace(r *http.Request) {
	c := trace.FromContext(r.Context())
	if c == nil {
		return
	}
	cfg := h.currentConfig()
	c.SetContext(trace.Context{RequestID: requestIDFromContext(r.Context()), Revision: executionRevision, Instance: executionInstance, ArchiveEnabled: cfg.ArchiveInteractions, ArchiveFullContent: cfg.ArchiveInteractions && cfg.ArchiveFullContent, RequestTimeoutSeconds: int(cfg.RequestTimeout / time.Second), MaxSSELineBytes: int(cfg.MaxSSELineBytes), WebsocketMaxMessageBytes: cfg.CodexOAuth.WebsocketMaxMessageBytes})
	h.checkpointExecutionTrace(r, true)
}
func (h *Handler) checkpointExecutionTrace(r *http.Request, force bool) {
	if r == nil || h.usageStore == nil {
		return
	}
	c := trace.FromContext(r.Context())
	completion := usageCompletionFromContext(r.Context())
	if c == nil || completion == nil {
		return
	}
	completion.traceMu.Lock()
	defer completion.traceMu.Unlock()
	if !force && time.Since(completion.traceCheckpointAt) < 5*time.Second {
		return
	}
	completion.traceCheckpointAt = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := h.usageStore.WriteTrace(ctx, usageEventIDFromContext(r.Context()), c.Snapshot()); err != nil {
		c.MarkSaveFailed()
		slog.Warn("execution trace checkpoint failed", "event_id", usageEventIDFromContext(r.Context()), "request_id", requestIDFromContext(r.Context()), "error", err)
	}
}
func (h *Handler) observeCodexExecution(r *http.Request, attempt codexresponses.HTTPAttempt, attemptErr error) {
	if r == nil {
		return
	}
	c := trace.FromContext(r.Context())
	if c == nil {
		return
	}
	if f, ok := attemptErr.(*codexresponses.Failure); ok && f == nil {
		attemptErr = nil
	}
	if c.CurrentID() == 0 {
		target := usagetarget.Snapshot(r.Context())
		c.Begin("codexoauth", target.AccountID, target.AccountLabel, attempt.Response.UpstreamRequestModel)
	}
	a := trace.Attempt{ObservedAt: time.Now().UTC(), Finished: attempt.Finished || attemptErr != nil, Failed: attemptErr != nil, Stage: "read", Transport: attempt.Response.Transport, FallbackReason: attempt.Response.FallbackReason, UpstreamStatus: attempt.Response.Status, Model: attempt.Response.UpstreamRequestModel, ResponseModel: attempt.Response.ResponseModel, QueueWaitMS: attempt.Request.QueueWaitMS, HeadersDurationMS: attempt.Response.DurationMS, ReadDurationMS: attempt.Response.ReadDurationMS, FirstEventDurationMS: attempt.Response.FirstEventDurationMS, TerminalEvent: attempt.Response.TerminalEvent, EventCount: attempt.Response.EventCount, WireBytes: attempt.Response.WireBytes, TransportReason: attempt.Response.TransportReason.Safe(), WebsocketDiagnostics: attempt.Response.WebsocketDiagnostics.Safe(), ErrorBodyState: "not_collected"}
	a.ReadObserved = attempt.Response.ReadObserved
	a.HeadersObserved = attempt.Response.Status > 0
	if a.Transport == "websocket" {
		sent := attempt.Response.WebsocketRequestSent
		a.RequestSent = &sent
	}
	if !attempt.Response.FirstOutputAt.IsZero() {
		at := attempt.Response.FirstOutputAt
		a.FirstOutputAt = &at
	}
	if !attempt.Response.LastEventAt.IsZero() {
		at := attempt.Response.LastEventAt
		a.LastEventAt = &at
	}
	if !attempt.Response.ReadObserved {
		a.Stage = "handshake"
	}
	if a.Finished {
		a.Stage = "complete"
	}
	if a.TerminalEvent != "" {
		a.Stage = "terminal"
	}
	if a.WebsocketFailureStage != "" {
		a.Stage = a.WebsocketFailureStage
	}
	if failure, ok := codexresponses.AsFailure(attemptErr); ok {
		a.ErrorCode = string(failure.Kind)
		a.FailureClass = codexFailureClass(failure)
		if a.FailureClass == "" {
			a.FailureClass = string(failure.Kind)
		}
		a.UpstreamError = aetherrelaycodex.UpstreamError{UpstreamErrorType: failure.UpstreamType, UpstreamErrorCode: failure.UpstreamCode, UpstreamErrorParam: failure.UpstreamParam, UpstreamErrorMessage: failure.UpstreamMessage}
	}
	for _, head := range attempt.Response.Headers {
		if strings.EqualFold(head.Name, "x-request-id") || strings.EqualFold(head.Name, "request-id") {
			a.UpstreamRequestID = head.Value
			break
		}
	}
	if len(attempt.Prior) > 0 {
		p := attempt.Prior[0]
		a.Handshakes = []trace.Handshake{{At: p.Request.At, Status: p.Response.Status, DurationMS: p.Response.DurationMS, TransportReason: p.Response.TransportReason.Safe()}}
	}
	// Raw bodies remain governed by the existing interaction archive policy.
	// A policy flag alone is never presented as proof that a file was saved.
	if len(attempt.Response.ErrorBody) > 0 {
		a.ErrorBodyState = "not_saved"
		if attempt.Response.ErrorBodyTruncated {
			a.ErrorBodyState = "truncated_not_saved"
		}
	}
	if attempt.Response.ErrorBodyReadFailed {
		a.ErrorBodyState = "read_failed"
	}
	c.Observe(a)
	h.checkpointExecutionTrace(r, a.Finished || a.Failed)
}
func (h *Handler) beginHTTPExecution(r *http.Request, provider string) {
	if r == nil {
		return
	}
	if c := trace.FromContext(r.Context()); c != nil {
		target := usagetarget.Snapshot(r.Context())
		c.Begin(provider, target.AccountID, target.AccountLabel, "")
		h.checkpointExecutionTrace(r, true)
	}
}
func (h *Handler) observeHTTPExecution(r *http.Request, resp *http.Response, d time.Duration, err error) {
	if r == nil {
		return
	}
	if c := trace.FromContext(r.Context()); c != nil && c.CurrentID() > 0 {
		a := trace.Attempt{ObservedAt: time.Now().UTC(), Stage: "read", Transport: "http", HeadersDurationMS: d.Milliseconds(), Failed: err != nil, Finished: err != nil, ErrorBodyState: "not_collected"}
		if resp != nil {
			a.HeadersObserved = true
			a.UpstreamStatus = resp.StatusCode
			a.UpstreamRequestID = resp.Header.Get("X-Request-Id")
			if resp.StatusCode >= 400 {
				a.Failed = true
				a.Finished = true
				a.FailureClass = "upstream_http"
			}
		}
		if err != nil {
			a.ErrorCode = "network"
		}
		c.Observe(a)
		h.checkpointExecutionTrace(r, a.Finished)
	}
}
func (h *Handler) finishExecutionTrace(r *http.Request, rec *usage.CompleteRecord) {
	if r == nil {
		return
	}
	c := trace.FromContext(r.Context())
	if c == nil {
		return
	}
	t := c.Snapshot()
	// Do not create an execution for conversion/admission failures that never
	// accessed an upstream. Generic HTTP has no terminal callback; finish only
	// its current unfinished observation from the authoritative settlement.
	if len(t.Attempts) > 0 {
		a := t.Attempts[len(t.Attempts)-1]
		if !a.Finished {
			a.Finished = true
			a.ObservedAt = rec.CompletedAt
			a.Failed = rec.Outcome != "success"
			a.Stage = "complete"
			a.FailureClass = rec.FailureClass
			a.ErrorCode = rec.ErrorCode
			a.UpstreamError = rec.UpstreamError
			if !rec.FirstOutputAt.IsZero() {
				at := rec.FirstOutputAt
				a.FirstOutputAt = &at
			}
			c.Observe(a)
		}
	}
	if rec.Outcome != "success" {
		if !rec.FirstOutputAt.IsZero() || rec.GenerationPartial {
			c.SuppressReplay("business_output_already_delivered")
		} else if len(t.Attempts) > 0 && t.Attempts[len(t.Attempts)-1].RequestSent != nil && *t.Attempts[len(t.Attempts)-1].RequestSent {
			c.SuppressReplay("request_sent_completion_uncertain")
		} else if rec.Retryable != nil && !*rec.Retryable {
			c.SuppressReplay("not_retryable")
		} else {
			c.SuppressReplay("no_further_execution_observed")
		}
	}
	t = c.Snapshot()
	rec.Trace = &t
}
