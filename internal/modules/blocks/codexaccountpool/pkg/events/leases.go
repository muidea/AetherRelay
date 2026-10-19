package events

import "time"

const (
	TopicLeaseSnapshot  = "aetherrelay.codex.accountpool.command.lease_snapshot"
	TopicLeaseState     = "aetherrelay.codex.accountpool.command.lease_state"
	TopicReleaseFailure = "aetherrelay.codex.accountpool.command.release_failure"
	LeaseHTTP           = "http"
	LeaseWebsocket      = "websocket"
	LeaseProbe          = "probe"
	LeaseActive         = "active"
	LeaseIdle           = "idle"
)

// LeaseRequest identifies execution without carrying request bodies or credentials.
type LeaseRequest struct {
	RequestID string `json:"request_id"`
	EventID   string `json:"event_id"`
	APIKeyID  string `json:"api_key_id"`
	Protocol  string `json:"protocol"`
	Endpoint  string `json:"endpoint"`
}

type LeaseView struct {
	LeaseRequest
	LeaseID        string    `json:"lease_id"`
	Model          string    `json:"model"`
	Transport      string    `json:"transport"`
	Kind           string    `json:"kind"`
	State          string    `json:"state"`
	AcquiredAt     time.Time `json:"acquired_at"`
	StateChangedAt time.Time `json:"state_changed_at"`
	HeldMS         int64     `json:"held_ms"`
	IdleMS         int64     `json:"idle_ms"`
}

type AccountSlots struct {
	AccountID       string      `json:"account_id"`
	AccountLabel    string      `json:"account_label"`
	MaxConcurrency  int         `json:"max_concurrency"`
	Inflight        int         `json:"inflight"`
	WebsocketActive int         `json:"websocket_active"`
	WebsocketIdle   int         `json:"websocket_idle"`
	OldestLeaseMS   int64       `json:"oldest_lease_ms"`
	ReleaseFailures uint64      `json:"release_failures"`
	Leases          []LeaseView `json:"leases"`
}

type LeaseSnapshotCommand struct{}
type LeaseSnapshotResult struct {
	At       time.Time      `json:"at"`
	Accounts []AccountSlots `json:"accounts"`
}

// State observations never acquire/release slots or alter admission policy.
type LeaseStateCommand struct {
	LeaseID string
	State   string
	Request LeaseRequest
}
type LeaseStateResult struct{ Updated bool }

type ReleaseFailureCommand struct {
	LeaseID   string
	AccountID string
}
type ReleaseFailureResult struct{}
