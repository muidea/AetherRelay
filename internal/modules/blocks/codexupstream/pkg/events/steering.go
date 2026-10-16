package events

import (
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/steering"
	"time"
)

const (
	TopicSteeringStart    = "aetherrelay.codex.upstream.command.steering_start"
	TopicSteeringSnapshot = "aetherrelay.codex.upstream.query.steering_snapshot"
	TopicSteeringClose    = "aetherrelay.codex.upstream.command.steering_close"
)

// Credentials remain internal. Probe IDs are supplied by the orchestrator so
// even a lost start acknowledgement can be closed without a replay.
type SteeringStartCommand struct {
	ProbeID         string
	Model           string
	Deadline        time.Time
	AccessToken     string
	AccountIDHeader string
	Proxy           string
	SessionHash     string
	Fingerprint     CodexFingerprint
	ClientIdentity  ClientIdentity
}
type SteeringStartResult struct{ ProbeID string }
type SteeringSnapshotCommand struct{ ProbeID string }
type SteeringSnapshotResult struct {
	Evidence steering.Evidence
	Endpoint string
	Headers  []Header
}
type SteeringCloseCommand struct{ ProbeID string }
type SteeringCloseResult struct {
	Closed   bool
	Snapshot SteeringSnapshotResult
}
