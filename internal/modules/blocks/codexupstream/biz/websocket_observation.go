package biz

import (
	"bytes"
	"time"

	events "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
)

func (s *websocketSession) beginObservation(payload []byte) events.HTTPAttempt {
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	s.turn = &responseStream{readStarted: time.Now(), progress: events.HTTPResponseObservation{WebsocketDiagnostics: s.handshake.Response.WebsocketDiagnostics, Observation: codexmodel.New(requestBodyModel(payload), "")}}
	s.attempt = s.handshake
	s.attempt.Request.At, s.attempt.Request.Method = time.Now(), "WS"
	s.attempt.Request.BodyBytes = len(payload)
	if s.archiveFullContent {
		s.attempt.Request.Body = bytes.Clone(payload)
	}
	s.attempt.Response.DurationMS = 0
	s.attempt.Response.WebsocketRequestSent = true
	s.published = false
	s.diagnosticPublished = false
	return s.attempt
}

// Publish full attempt bodies at the first event, first diagnostic and terminal.
// Copying input into every token update would amplify memory with output length.
func (s *websocketSession) observePayload(payload []byte, failedRead bool) events.HTTPAttempt {
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if s.turn == nil {
		return events.HTTPAttempt{}
	}
	line := append(append([]byte("data: "), payload...), '\n', '\n')
	terminal := terminalEventType(line) != ""
	if len(payload) > 0 {
		s.turn.observe(line)
	}
	if terminal || failedRead {
		s.turn.finish()
	}
	progress := s.turn.observation()
	if s.published && !terminal && !failedRead && (progress.WebsocketFailureStage == "" || s.diagnosticPublished) {
		return events.HTTPAttempt{}
	}
	s.published = true
	s.diagnosticPublished = progress.WebsocketFailureStage != ""
	attempt := s.attempt
	attempt.Response.Observation = progress.Observation
	attempt.Response.WebsocketDiagnostics = progress.WebsocketDiagnostics
	attempt.Response.TransportReason = progress.TransportReason
	attempt.Response.ReadObserved, attempt.Response.ReadDurationMS = progress.ReadObserved, progress.ReadDurationMS
	attempt.Response.FirstOutputAt, attempt.Response.GenerationDuration, attempt.Response.GenerationPartial = progress.FirstOutputAt, progress.GenerationDuration, progress.GenerationPartial
	attempt.Response.FirstEventDurationMS, attempt.Response.LastEventAt, attempt.Response.LastEventDurationMS = progress.FirstEventDurationMS, progress.LastEventAt, progress.LastEventDurationMS
	attempt.Response.TerminalEvent, attempt.Response.EventCount, attempt.Response.WireBytes = progress.TerminalEvent, progress.EventCount, progress.WireBytes
	return attempt
}
