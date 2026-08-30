// Package aetherrelayproxy owns the shared account-proxy transport policy.
package aetherrelayproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ParseHTTPURL accepts the account proxy schemes supported by AetherRelay.
func ParseHTTPURL(raw string) (*url.URL, error) {
	proxyURL, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || proxyURL.Host == "" || proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, fmt.Errorf("invalid HTTP proxy URL")
	}
	return proxyURL, nil
}

// NewHTTPTransport clones the process default and applies the account proxy.
// For an HTTPS proxy, its TLS leg is explicitly HTTP/1.1 because CONNECT is
// an HTTP/1.1 exchange; negotiating h2 here produces invalid proxy greetings.
func NewHTTPTransport(raw string) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		return nil, fmt.Errorf("default HTTP transport is unavailable")
	}
	transport := base.Clone()
	if strings.TrimSpace(raw) == "" {
		return transport, nil
	}
	proxyURL, err := ParseHTTPURL(raw)
	if err != nil {
		return nil, err
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	if proxyURL.Scheme == "https" {
		transport.DialTLSContext = HTTPSProxyDialTLSContext(proxyURL, transport.TLSClientConfig, transport.TLSHandshakeTimeout, transport.DialContext)
	}
	return transport, nil
}

// HTTPSProxyDialTLSContext performs the TLS handshake with a fixed HTTPS
// proxy. The backend TLS handshake still belongs to net/http or the websocket
// dialer after CONNECT succeeds.
func HTTPSProxyDialTLSContext(proxyURL *url.URL, baseTLS *tls.Config, handshakeTimeout time.Duration, baseDialContext func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	proxyHostname := ""
	if proxyURL != nil {
		proxyHostname = proxyURL.Hostname()
	}
	dialContext := baseDialContext
	if dialContext == nil {
		dialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		rawConn, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConfig := &tls.Config{}
		if baseTLS != nil {
			tlsConfig = baseTLS.Clone()
		}
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = proxyHostname
		}
		tlsConfig.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(rawConn, tlsConfig)
		handshakeContext := ctx
		if handshakeTimeout > 0 {
			var cancel context.CancelFunc
			handshakeContext, cancel = context.WithTimeout(ctx, handshakeTimeout)
			defer cancel()
		}
		if err := tlsConn.HandshakeContext(handshakeContext); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("HTTPS proxy TLS handshake failed: %w", err)
		}
		return tlsConn, nil
	}
}
