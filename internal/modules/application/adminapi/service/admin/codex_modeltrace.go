package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	detectionevents "aetherrelay/internal/modules/application/modeldetection/pkg/events"
)

type codexModelTraceRuntime interface {
	StartCodexModelTraceProbe(context.Context, string, string, string, string) (detectionevents.Report, error)
	GetCodexModelTraceProbe(context.Context, string, string) (detectionevents.Report, error)
	CancelCodexModelTraceProbe(context.Context, string, string) (detectionevents.Report, error)
}

// Returns false for unrelated routes. Exact path shape keeps these operations
// on the authenticated management surface and rejects cross-slot lookups.
func (h *Handler) codexModelTraceAPI(w http.ResponseWriter, r *http.Request, rel string) bool {
	parts := strings.Split(strings.TrimPrefix(rel, "/api/codex/accounts/"), "/")
	if !strings.HasPrefix(rel, "/api/codex/accounts/") || len(parts) < 2 || parts[1] != "modeltrace-probes" {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	if (len(parts) != 2 || r.Method != http.MethodPost) && (len(parts) != 3 || (r.Method != http.MethodGet && r.Method != http.MethodDelete)) || parts[0] == "" {
		writeError(w, 404, "not found")
		return true
	}
	if r.Method != http.MethodGet && !h.requireAdminMutation(w, r) {
		return true
	}
	runtime, ok := h.codex.(codexModelTraceRuntime)
	if !ok {
		writeError(w, 503, "probe_unavailable")
		return true
	}
	var report detectionevents.Report
	var err error
	status := http.StatusOK
	if r.Method == http.MethodPost {
		var body struct {
			Model     string `json:"model"`
			Reasoning string `json:"reasoning"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
			writeError(w, 400, "invalid probe json; only model and reasoning are accepted")
			return true
		}
		if body.Model == "" || strings.TrimSpace(body.Model) != body.Model || len(body.Model) > 256 {
			writeError(w, 400, "model is required")
			return true
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 128 || strings.ContainsAny(key, "\r\n\x00") {
			writeError(w, 400, "Idempotency-Key is required (at most 128 bytes)")
			return true
		}
		report, err = runtime.StartCodexModelTraceProbe(r.Context(), parts[0], body.Model, body.Reasoning, key)
		status = http.StatusAccepted
	} else if r.Method == http.MethodDelete {
		report, err = runtime.CancelCodexModelTraceProbe(r.Context(), parts[0], parts[2])
		status = http.StatusAccepted
	} else {
		report, err = runtime.GetCodexModelTraceProbe(r.Context(), parts[0], parts[2])
	}
	if err != nil {
		var apiError *detectionevents.APIError
		if errors.As(err, &apiError) {
			writeError(w, apiError.Status, apiError.Code)
		} else {
			writeError(w, 503, "probe_unavailable")
		}
		return true
	}
	writeJSON(w, status, report)
	return true
}
