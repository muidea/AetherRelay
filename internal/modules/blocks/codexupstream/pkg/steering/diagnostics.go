package steering

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxTimelineEvents = 64

// UpstreamError is a bounded projection of error fields, never a whole event.
// Status belongs to this failure; HTTPStatus in Evidence is the WS handshake.
type UpstreamError struct {
	EventType string `json:"event_type"`
	Type      string `json:"type,omitempty"`
	Code      string `json:"code,omitempty"`
	Param     string `json:"param,omitempty"`
	Message   string `json:"message,omitempty"`
	Status    int    `json:"status,omitempty"`
	Truncated bool   `json:"truncated"`
}

type TraceEvent struct {
	Sequence  int64  `json:"sequence"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Direction string `json:"direction"`
	Type      string `json:"type"`
	State     string `json:"state,omitempty"`
}

func boundedText(value string, limit int) (string, bool) {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if len(clean) <= limit {
		if clean != value {
			return strings.Clone(clean), true
		}
		return clean, false
	}
	end := limit
	for !utf8.ValidString(clean[:end]) {
		end--
	}
	// Do not let a short report field retain a whole oversized event string.
	return strings.Clone(clean[:end]), true
}

func (m *Machine) Trace(direction, kind, state string) {
	m.sequence++
	kind, shortened := boundedText(kind, 128)
	if shortened {
		m.TimelineTruncated = true
	}
	e := TraceEvent{Sequence: m.sequence, ElapsedMS: time.Since(m.started).Milliseconds(), Direction: direction, Type: kind, State: state}
	if len(m.Timeline) == MaxTimelineEvents {
		copy(m.Timeline, m.Timeline[1:])
		m.Timeline = m.Timeline[:MaxTimelineEvents-1]
		m.TimelineTruncated = true
	}
	m.Timeline = append(m.Timeline, e)
}

// WriteAttempt retains the existing *_sent meaning: the write may have reached
// the upstream even if it subsequently fails. Completion is separate evidence.
func (m *Machine) WriteAttempt(kind string) {
	if kind == "response.create" {
		m.CreateSent = true
	}
	if kind == "response.steer" {
		m.SteerSent = true
		m.SteeringState = "writing"
	}
	m.Trace("send", kind, "attempt")
}

func (m *Machine) WriteResult(kind string, err error) {
	state := "write_failed"
	if err == nil {
		state = "written"
	}
	if kind == "response.create" {
		m.CreateWriteCompleted = err == nil
	}
	if kind == "response.steer" {
		m.SteerWriteCompleted = err == nil
		m.SteeringState = "write_failed"
		if err == nil {
			m.SteeringState = "sent"
		}
	}
	m.Trace("send", kind, state)
}

func (m *Machine) RecordUpstreamError(detail UpstreamError) {
	for _, field := range []struct {
		text  *string
		limit int
	}{
		{&detail.EventType, 128}, {&detail.Type, 128}, {&detail.Code, 128}, {&detail.Param, 256}, {&detail.Message, 4096},
	} {
		value, truncated := boundedText(*field.text, field.limit)
		*field.text = value
		detail.Truncated = detail.Truncated || truncated
	}
	if detail.Status < 400 || detail.Status > 599 {
		detail.Status = 0
	}
	m.UpstreamError = &detail
}

type errorWire struct {
	Type    json.RawMessage `json:"type"`
	Code    json.RawMessage `json:"code"`
	Param   json.RawMessage `json:"param"`
	Message json.RawMessage `json:"message"`
	Status  json.RawMessage `json:"status"`
}

func scalarText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil && number != "" {
		return string(number)
	}
	return ""
}

func errorStatus(raw json.RawMessage) int {
	var value int
	if json.Unmarshal(raw, &value) != nil || value < 400 || value > 599 {
		return 0
	}
	return value
}

func (m *Machine) eventError(kind string, e errorWire, status json.RawMessage, fallbackCode json.RawMessage) string {
	code := scalarText(e.Code)
	if code == "" {
		code = scalarText(fallbackCode)
	}
	detail := UpstreamError{EventType: kind, Type: scalarText(e.Type), Code: code, Param: scalarText(e.Param), Message: scalarText(e.Message), Status: errorStatus(status)}
	if detail.Status == 0 {
		detail.Status = errorStatus(e.Status)
	}
	m.RecordUpstreamError(detail)
	if code == "" {
		code = detail.Type
	}
	return code
}

// Clone prevents published snapshots and admin readers from sharing storage
// with the event reader or with one another.
func (e Evidence) Clone() Evidence {
	e.RequiredInput = append([]string(nil), e.RequiredInput...)
	e.Timeline = append([]TraceEvent(nil), e.Timeline...)
	if e.UpstreamError != nil {
		v := *e.UpstreamError
		e.UpstreamError = &v
	}
	cloneUsage := func(u *Usage) *Usage {
		if u == nil {
			return nil
		}
		v := *u
		for _, ptr := range []**int64{&v.InputTokens, &v.OutputTokens, &v.TotalTokens} {
			if *ptr != nil {
				n := **ptr
				*ptr = &n
			}
		}
		return &v
	}
	e.ParentUsage, e.SuccessorUsage = cloneUsage(e.ParentUsage), cloneUsage(e.SuccessorUsage)
	return e
}
