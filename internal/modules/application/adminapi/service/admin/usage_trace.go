package admin

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func (h *Handler) usageTrace(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("event_id")
	f := usage.EventFilter{UsageFilter: usage.UsageFilter{AllTime: true}, EventIDs: []string{id}, PageSize: 1}
	if usage.ValidateEventFilter(&f) != nil {
		writeError(w, http.StatusBadRequest, "invalid event_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	page, err := h.usageStore.Events(ctx, f)
	h.observeUsageQuery(started, err)
	if err != nil {
		writeError(w, 503, "usage query failed")
		return
	}
	if len(page.Events) == 0 {
		writeError(w, 404, "usage event not found")
		return
	}
	t, err := h.usageStore.ExecutionTrace(ctx, id)
	if err != nil {
		writeError(w, 503, "execution trace query failed")
		return
	}
	detail := usage.BuildTraceDetail(page.Events[0], t)
	if h.runtime != nil && detail.Event.RoundID > 0 {
		cfg := h.runtime.ConfigSnapshot()
		metadata, relative := archive.InspectEvidence(cfg.InteractionDir, detail.Event.APIKeyID, detail.Event.RoundID)
		detail.Evidence = append(detail.Evidence, usage.EvidenceField{Field: "interaction_archive", State: metadata, Source: relative})
		for i := range detail.Evidence {
			f := &detail.Evidence[i]
			if f.Field == "raw_error_body" && (f.State == "archived" || f.State == "archived_truncated") {
				name := ""
				if t != nil {
					for _, a := range t.Attempts {
						if fmt.Sprint(a.ID) == f.Detail {
							name = a.ErrorBodyFile
							break
						}
					}
				}
				raw := archive.InspectErrorEvidence(cfg.InteractionDir, detail.Event.APIKeyID, detail.Event.RoundID, name)
				if raw == "saved" {
					continue
				}
				f.State = "cleared_or_missing"
				if raw == "read_failed" {
					f.State = raw
				}
			}
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="execution-diagnostic.json"`)
	}
	writeJSON(w, 200, detail)
}
func (h *Handler) usageAnomalies(w http.ResponseWriter, r *http.Request) {
	f, err := parseUsageFilter(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	group := strings.TrimSpace(r.URL.Query().Get("group_by"))
	if group == "" {
		group = "cause"
	}
	switch group {
	case "cause", "account", "model", "revision", "transport", "provider":
	default:
		writeError(w, 400, "invalid group_by")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	out, err := h.usageStore.Anomalies(ctx, f, group)
	h.observeUsageQuery(started, err)
	if err != nil {
		writeError(w, 503, "anomaly query failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

const usageCSVMaxBytes = 32 << 20

type usageCSVBuffer struct{ bytes.Buffer }

func (b *usageCSVBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > usageCSVMaxBytes {
		return 0, fmt.Errorf("export size limit exceeded")
	}
	return b.Buffer.Write(p)
}
func (h *Handler) usageEventsCSV(w http.ResponseWriter, r *http.Request) {
	f, err := parseUsageFilter(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	n, err := h.usageStore.Count(ctx, f)
	if err != nil {
		writeError(w, 503, "usage query failed")
		return
	}
	if n > 10000 {
		writeError(w, 400, "export limited to 10000 events; narrow the filters")
		return
	}
	var buf usageCSVBuffer
	if err = h.usageStore.ExportCSV(ctx, f, &buf); err != nil {
		writeError(w, 503, "usage export failed; narrow the filters")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="usage-events.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}
