package biz

import (
	"context"
	"encoding/json"
	"errors"

	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	wsclient "github.com/bogdanfinn/websocket"
)

type websocketDiagnosticError struct {
	cause      error
	diagnostic transport.WebsocketDiagnostics
	reason     transport.Reason
}

func (e *websocketDiagnosticError) Error() string { return e.cause.Error() }
func (e *websocketDiagnosticError) Unwrap() error { return e.cause }

func websocketFailure(ctx context.Context, stage string, err error, previous transport.WebsocketDiagnostics) *websocketDiagnosticError {
	if ctx == nil {
		ctx = context.Background()
	}
	d := previous
	d.WebsocketFailureStage = stage
	reason := transport.Classify(err)
	var closeErr *wsclient.CloseError
	var syntax *json.SyntaxError
	var valueType *json.UnmarshalTypeError
	switch {
	case ctx.Err() != nil:
		reason = transport.Classify(ctx.Err())
		d.WebsocketErrorType = string(reason)
	case errors.Is(err, wsclient.ErrReadLimit):
		d.WebsocketErrorType, reason = "message_limit", transport.WebsocketMessageLimit
	case errors.As(err, &closeErr):
		d.WebsocketErrorType, reason = "close", transport.WebsocketClose
		d.WebsocketCloseCode = closeErr.Code
		d.WebsocketCloseReceived = closeErr.Code != 1006 && closeErr.Code != 1015
	case errors.As(err, &syntax):
		d.WebsocketErrorType, reason, d.WebsocketJSONErrorOffset = "json_syntax", transport.WebsocketInvalidJSON, syntax.Offset
	case errors.As(err, &valueType):
		d.WebsocketErrorType, reason, d.WebsocketJSONErrorOffset = "json_type", transport.WebsocketInvalidJSON, valueType.Offset
	case stage == "decode":
		d.WebsocketErrorType, reason = "json_invalid", transport.WebsocketInvalidJSON
	case reason == transport.Timeout:
		d.WebsocketErrorType = "timeout"
	case reason == transport.Canceled:
		d.WebsocketErrorType = "canceled"
	case reason != transport.Unknown:
		d.WebsocketErrorType = "network"
	case stage == "write":
		d.WebsocketErrorType, reason = "write_error", transport.WebsocketWriteError
	default:
		d.WebsocketErrorType, reason = "read_error", transport.WebsocketReadError
	}
	return &websocketDiagnosticError{cause: err, diagnostic: previous.Merge(d), reason: reason}
}

func (s *responseStream) observeWebsocketFailure(err error) {
	d := websocketDiagnostic(err)
	if d.WebsocketFailureStage == "" {
		return
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.progress.WebsocketDiagnostics = s.progress.WebsocketDiagnostics.Merge(d)
	if s.progress.TransportReason == "" {
		s.progress.TransportReason = transportReason(err)
	}
}

func (s *websocketSession) noteWebsocketMessage(payload []byte) {
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if s.turn == nil {
		return
	}
	s.turn.progressMu.Lock()
	defer s.turn.progressMu.Unlock()
	s.turn.progress.WebsocketDiagnostics = s.turn.progress.WebsocketDiagnostics.Merge(transport.WebsocketDiagnostics{
		WebsocketLastMessageObserved: true, WebsocketLastMessageBytes: int64(len(payload)),
	})
}

func (s *websocketSession) noteWebsocketFailure(ctx context.Context, stage string, err error) {
	s.observationMu.Lock()
	defer s.observationMu.Unlock()
	if s.turn == nil {
		return
	}
	progress := s.turn.observation()
	if progress.TerminalEvent != "" {
		return // An idle connection closing cannot fail an already settled turn.
	}
	s.turn.observeWebsocketFailure(websocketFailure(ctx, stage, err, progress.WebsocketDiagnostics))
}

func websocketDiagnostic(err error) transport.WebsocketDiagnostics {
	var observed *websocketDiagnosticError
	if errors.As(err, &observed) {
		return observed.diagnostic.Safe()
	}
	return transport.WebsocketDiagnostics{}
}
