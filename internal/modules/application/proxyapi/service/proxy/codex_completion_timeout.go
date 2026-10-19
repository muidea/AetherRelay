package proxy

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
)

type codexCompletionBudget struct {
	deadline      time.Time
	started       time.Time
	clientTimeout string
	source        string
}

// A buffered response writes no headers until the terminal upstream event.
// Leave time for serialization and error delivery before the declared client
// timeout. Invalid hints cannot extend the configured gateway budget.
func (h *Handler) codexCompletionDeadline(r *http.Request, started time.Time) time.Time {
	return h.codexCompletionBudget(r, started).deadline
}

func (h *Handler) codexCompletionBudget(r *http.Request, started time.Time) codexCompletionBudget {
	budget := codexCompletionBudget{source: "unbounded", started: started}
	if r != nil {
		budget.clientTimeout = r.Header.Get("X-Stainless-Timeout")
	}
	limit := func(deadline time.Time, source string) {
		if budget.deadline.IsZero() || deadline.Before(budget.deadline) {
			budget.deadline, budget.source = deadline, source
		}
	}
	if timeout := h.currentConfig().RequestTimeout; timeout > 0 {
		limit(started.Add(timeout), "server")
	}
	if r == nil {
		return budget
	}
	if deadline, ok := r.Context().Deadline(); ok {
		limit(deadline, "context")
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(r.Header.Get("X-Stainless-Timeout")), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64((24*time.Hour)/time.Second) {
		return budget
	}
	timeout := time.Duration(seconds * float64(time.Second))
	if timeout <= 0 {
		return budget
	}
	reserve := min(5*time.Second, timeout/20)
	limit(started.Add(timeout-reserve), "client")
	return budget
}

// Keep budget and request shape diagnostics separate from full-content archives.
// Prompts, tool schemas and metadata must never enter ordinary logs, even at DEBUG.
func (h *Handler) prepareCodexCompletion(r *http.Request, started time.Time, request *codexresponses.Request) {
	budget := h.codexCompletionBudget(r, started)
	request.Deadline = budget.deadline
	if completion := usageCompletionFromContext(r.Context()); completion != nil {
		completion.budgetMu.Lock()
		completion.completionBudget = budget
		completion.budgetMu.Unlock()
	}
	if !slog.Default().Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	var shape struct {
		Input     json.RawMessage   `json:"input"`
		Tools     []json.RawMessage `json:"tools"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	_ = json.Unmarshal(request.Body, &shape)
	var input []json.RawMessage
	_ = json.Unmarshal(shape.Input, &input)
	budgetMS := int64(0)
	if !budget.deadline.IsZero() {
		budgetMS = max(0, budget.deadline.Sub(started).Milliseconds())
	}
	// These fields are client-controlled too; keep diagnostics bounded without
	// modifying the request or the timeout calculation.
	effort := shape.Reasoning.Effort
	if len(effort) > 128 {
		effort = "<oversized>"
	}
	clientTimeout := safeCodexClientTimeout(budget.clientTimeout)
	slog.DebugContext(r.Context(), "Codex completion budget", "request_id", request.Diagnostics.RequestID,
		"request_budget_ms", budgetMS, "request_budget_source", budget.source,
		"request_bytes", len(request.Body), "input_items", len(input), "tool_count", len(shape.Tools),
		"reasoning_effort", effort,
		"client_timeout", clientTimeout)
}

func safeCodexClientTimeout(value string) string {
	clientTimeout := strings.TrimSpace(value)
	if clientTimeout != "" {
		seconds, err := strconv.ParseFloat(clientTimeout, 64)
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64((24*time.Hour)/time.Second) {
			clientTimeout = "<invalid>"
		} else {
			clientTimeout = strconv.FormatFloat(seconds, 'g', -1, 64)
		}
	}
	return clientTimeout
}

// Preserve the admitted budget when config or headers change during a request.
func (h *Handler) logCodexCompletionTimeout(r *http.Request, started time.Time) {
	var budget codexCompletionBudget
	if completion := usageCompletionFromContext(r.Context()); completion != nil {
		completion.budgetMu.Lock()
		budget = completion.completionBudget
		completion.budgetMu.Unlock()
	}
	if budget.started.IsZero() {
		budget = h.codexCompletionBudget(r, started)
	}
	budgetMS := int64(0)
	if !budget.deadline.IsZero() {
		budgetMS = max(0, budget.deadline.Sub(budget.started).Milliseconds())
	}
	requestID := ""
	if round := archiveRoundFromContext(r.Context()); round != nil {
		requestID = round.RequestID
	}
	slog.WarnContext(r.Context(), "Codex completion budget exceeded", "request_id", requestID,
		"request_budget_ms", budgetMS, "request_budget_source", budget.source,
		"client_timeout", budget.clientTimeout, "elapsed_ms", time.Since(budget.started).Milliseconds())
}
