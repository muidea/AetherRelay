package aetherrelaytransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"syscall"

	"golang.org/x/net/http2"
)

// Classify derives a stable reason from error types, independently of the
// original error retained by the caller.
func Classify(err error) Reason {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	var stream http2.StreamError
	if errors.As(err, &stream) {
		switch stream.Code {
		case http2.ErrCodeCancel:
			return HTTP2StreamCancel
		case http2.ErrCodeRefusedStream:
			return HTTP2RefusedStream
		default:
			return HTTP2Stream
		}
	}
	var goAway http2.GoAwayError
	if errors.As(err, &goAway) {
		return HTTP2GoAway
	}
	var connection http2.ConnectionError
	if errors.As(err, &connection) {
		return HTTP2Connection
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return DNS
	}
	var record tls.RecordHeaderError
	var verification *tls.CertificateVerificationError
	var alert tls.AlertError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var roots x509.SystemRootsError
	if errors.As(err, &record) || errors.As(err, &verification) || errors.As(err, &alert) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &roots) {
		return TLS
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return ConnectionRefused
	case errors.Is(err, syscall.ECONNRESET):
		return ConnectionReset
	case errors.Is(err, syscall.ECONNABORTED):
		return ConnectionAborted
	case errors.Is(err, syscall.ETIMEDOUT):
		return Timeout
	case errors.Is(err, syscall.EPIPE):
		return BrokenPipe
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return EOF
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return Timeout
	}
	return Unknown
}
