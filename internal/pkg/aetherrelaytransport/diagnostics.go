// Package aetherrelaytransport defines credential-safe transport diagnostic values.
package aetherrelaytransport

// Reason is observation only; it must never drive retry or account health policy.
type Reason string

const (
	DNS               Reason = "dns"
	ConnectionRefused Reason = "connection_refused"
	ConnectionReset   Reason = "connection_reset"
	ConnectionAborted Reason = "connection_aborted"
	BrokenPipe        Reason = "broken_pipe"
	TLS               Reason = "tls"
	EOF               Reason = "eof"
	Timeout           Reason = "timeout"
	Canceled          Reason = "canceled"
	Unknown           Reason = "unknown"
)

// Safe preserves absence and maps any unrecognized projection to Unknown.
func (r Reason) Safe() Reason {
	switch r {
	case "", DNS, ConnectionRefused, ConnectionReset, ConnectionAborted, BrokenPipe, TLS, EOF, Timeout, Canceled, Unknown:
		return r
	default:
		return Unknown
	}
}
