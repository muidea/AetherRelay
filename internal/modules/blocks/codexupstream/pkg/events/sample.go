package events

import "time"

const (
	TopicSampleStart    = "aetherrelay.codex.upstream.command.sample_start"
	TopicSampleSnapshot = "aetherrelay.codex.upstream.query.sample_snapshot"
	TopicSampleClose    = "aetherrelay.codex.upstream.command.sample_close"
)

// A bounded synthetic HTTP request; credentials stay within trusted owners.
// Caller supplies ProbeID, allowing cleanup after a lost start acknowledgement.
type SampleStartCommand struct {
	ProbeID  string
	Deadline time.Time
	Request  CompleteCommand
}
type SampleStartResult struct{ ProbeID string }
type SampleSnapshotCommand struct{ ProbeID string }
type SampleSnapshotResult struct {
	Done                   bool
	Body                   []byte
	Headers                []Header
	HTTPStatus             int
	ErrorClass             ErrorClass
	EffectiveRequestSHA256 string
}

func (s SampleSnapshotResult) Clone() SampleSnapshotResult {
	s.Body = append([]byte(nil), s.Body...)
	s.Headers = append([]Header(nil), s.Headers...)
	return s
}

type SampleCloseCommand struct{ ProbeID string }
type SampleCloseResult struct {
	Closed   bool
	Snapshot SampleSnapshotResult
}
