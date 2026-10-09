package proxy

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A buffered response writes no headers until the terminal upstream event.
// Leave time for serialization and error delivery before the declared client
// timeout. Invalid hints cannot extend the configured gateway budget.
func (h *Handler) codexCompletionDeadline(r *http.Request, started time.Time) time.Time {
	var deadline time.Time
	if timeout := h.currentConfig().RequestTimeout; timeout > 0 {
		deadline = started.Add(timeout)
	}
	if r == nil {
		return deadline
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(r.Header.Get("X-Stainless-Timeout")), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64((24*time.Hour)/time.Second) {
		return deadline
	}
	timeout := time.Duration(seconds * float64(time.Second))
	if timeout <= 0 {
		return deadline
	}
	reserve := min(5*time.Second, timeout/20)
	clientDeadline := started.Add(timeout - reserve)
	if deadline.IsZero() || clientDeadline.Before(deadline) {
		deadline = clientDeadline
	}
	return deadline
}
