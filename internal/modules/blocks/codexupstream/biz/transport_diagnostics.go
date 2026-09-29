package biz

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"syscall"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
)

// transportReason projects error types only. Error strings can contain proxy
// credentials, URLs and private addresses and never cross this owner boundary.
func transportReason(err error) transport.Reason {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return transport.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return transport.Timeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return transport.DNS
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
		return transport.TLS
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return transport.ConnectionRefused
	case errors.Is(err, syscall.ECONNRESET):
		return transport.ConnectionReset
	case errors.Is(err, syscall.ECONNABORTED):
		return transport.ConnectionAborted
	case errors.Is(err, syscall.ETIMEDOUT):
		return transport.Timeout
	case errors.Is(err, syscall.EPIPE):
		return transport.BrokenPipe
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return transport.EOF
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return transport.Timeout
	}
	return transport.Unknown
}

// Protocol/business errors from completedResponse are not transport failures.
func completedTransportReason(class events.ErrorClass, err error) transport.Reason {
	if class == events.ErrorNetwork || class == events.ErrorTimeout || errors.Is(err, io.EOF) {
		return transportReason(err)
	}
	return ""
}
