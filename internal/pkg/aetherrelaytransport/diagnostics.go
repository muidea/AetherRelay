// Package aetherrelaytransport defines credential-safe transport diagnostic values.
package aetherrelaytransport

// Reason is observation only; it must never drive retry or account health policy.
type Reason string

const (
	DNS                Reason = "dns"
	ConnectionRefused  Reason = "connection_refused"
	ConnectionReset    Reason = "connection_reset"
	ConnectionAborted  Reason = "connection_aborted"
	BrokenPipe         Reason = "broken_pipe"
	TLS                Reason = "tls"
	EOF                Reason = "eof"
	Timeout            Reason = "timeout"
	Canceled           Reason = "canceled"
	HTTP2Stream        Reason = "http2_stream_error"
	HTTP2StreamCancel  Reason = "http2_stream_cancel"
	HTTP2RefusedStream Reason = "http2_refused_stream"
	HTTP2GoAway        Reason = "http2_goaway"
	HTTP2Connection    Reason = "http2_connection_error"
	SSELineLimit       Reason = "sse_line_limit"
	Unknown            Reason = "unknown"
)

// Safe preserves absence and maps any unrecognized projection to Unknown.
func (r Reason) Safe() Reason {
	switch r {
	case "", DNS, ConnectionRefused, ConnectionReset, ConnectionAborted, BrokenPipe, TLS, EOF, Timeout, Canceled, HTTP2Stream, HTTP2StreamCancel, HTTP2RefusedStream, HTTP2GoAway, HTTP2Connection, SSELineLimit, Unknown:
		return r
	default:
		return Unknown
	}
}
