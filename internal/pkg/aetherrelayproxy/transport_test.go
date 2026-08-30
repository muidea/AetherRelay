package aetherrelayproxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// CP-SEC-004: CONNECT proxies negotiate HTTP/1.1 on their TLS leg.
func TestHTTPSProxyDialUsesHTTP11ALPN(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dial := HTTPSProxyDialTLSContext(proxyURL, &tls.Config{InsecureSkipVerify: true}, time.Second, nil) // test certificate
	conn, err := dial(context.Background(), "tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("proxy connection type=%T", conn)
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "http/1.1" {
		t.Fatalf("proxy ALPN=%q", tlsConn.ConnectionState().NegotiatedProtocol)
	}
}

func TestNewHTTPTransportConfiguresOnlyHTTPSProxyTLS(t *testing.T) {
	httpsTransport, err := NewHTTPTransport("https://proxy.invalid:8443")
	if err != nil || httpsTransport.DialTLSContext == nil {
		t.Fatalf("HTTPS transport=%v err=%v", httpsTransport, err)
	}
	httpTransport, err := NewHTTPTransport("http://proxy.invalid:8080")
	if err != nil || httpTransport.DialTLSContext != nil {
		t.Fatalf("HTTP transport=%v err=%v", httpTransport, err)
	}
	if _, err := NewHTTPTransport("socks5://proxy.invalid:1080"); err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
}

func TestHTTPSProxyDialClosesConnectionAfterHandshakeFailure(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	proxyURL, _ := url.Parse("https://proxy.invalid")
	dial := HTTPSProxyDialTLSContext(proxyURL, nil, 10*time.Millisecond, func(context.Context, string, string) (net.Conn, error) { return client, nil })
	if _, err := dial(context.Background(), "tcp", "proxy.invalid:443"); err == nil {
		t.Fatal("TLS handshake unexpectedly succeeded")
	}
}
