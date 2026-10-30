package routeregistry

import (
	"log/slog"
	"net/http"
	"sort"
	"time"
)

type pendingRequest struct {
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	StartedAt time.Time `json:"started_at"`
	ElapsedMS int64     `json:"elapsed_ms"`
}

func (r *routeRegistry) trackRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if r.requests == nil {
			r.requests = make(map[uint64]pendingRequest)
		}
		r.requestSequence++
		id := r.requestSequence
		r.requests[id] = pendingRequest{Method: req.Method, Path: req.URL.Path, StartedAt: time.Now().UTC()}
		r.mu.Unlock()
		defer func() { r.mu.Lock(); delete(r.requests, id); r.mu.Unlock() }()
		next.ServeHTTP(w, req)
	})
}

func (r *routeRegistry) logPendingRequests() {
	r.mu.Lock()
	count := len(r.requests)
	requests := make([]pendingRequest, 0, count)
	for _, req := range r.requests {
		req.ElapsedMS = time.Since(req.StartedAt).Milliseconds()
		requests = append(requests, req)
	}
	r.mu.Unlock()
	sort.Slice(requests, func(i, j int) bool { return requests[i].StartedAt.Before(requests[j].StartedAt) })
	if len(requests) > 20 {
		requests = requests[:20]
	}
	slog.Warn("HTTP shutdown still waiting", "stage", "http.quiesce", "pending_requests", count, "oldest_requests", requests)
}
