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
	deadline time.Time
	source   string
}

// A buffered response writes no headers until the terminal upstream event.
// Leave time for serialization and error delivery before the declared client
// timeout. Invalid hints cannot extend the configured gateway budget.
func (h *Handler) codexCompletionDeadline(r *http.Request, started time.Time) time.Time {
	return h.codexCompletionBudget(r, started).deadline
}

func (h *Handler) codexCompletionBudget(r *http.Request, started time.Time) codexCompletionBudget {
	budget := codexCompletionBudget{source: "unbounded"}
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

// Preserve the normalized request and actual reasoning parameters for diagnostics.
// Authentication headers are not part of the logged request body.
func (h *Handler) prepareCodexCompletion(r *http.Request, started time.Time, request *codexresponses.Request) {
	budget := h.codexCompletionBudget(r, started)
	request.Deadline = budget.deadline
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
	slog.InfoContext(r.Context(), "Codex completion budget", "request_id", request.Diagnostics.RequestID,
		"request_budget_ms", budgetMS, "request_budget_source", budget.source,
		"request_bytes", len(request.Body), "input_items", len(input), "tool_count", len(shape.Tools),
		"reasoning_effort", shape.Reasoning.Effort,
		"client_timeout", r.Header.Get("X-Stainless-Timeout"), "request_body", string(request.Body))
}
