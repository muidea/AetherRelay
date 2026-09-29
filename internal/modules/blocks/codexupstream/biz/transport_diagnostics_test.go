package biz

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	"github.com/muidea/magicCommon/event"
)

func TestTransportReasonUsesErrorChainWithoutErrorText(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want transport.Reason
	}{
		{"absent", nil, ""},
		{"dns", &net.DNSError{Name: "private-host", Err: "private-error"}, transport.DNS},
		{"dns_timeout", &net.DNSError{Name: "private-host", IsTimeout: true}, transport.DNS},
		{"refused", syscall.ECONNREFUSED, transport.ConnectionRefused},
		{"reset", syscall.ECONNRESET, transport.ConnectionReset},
		{"aborted", syscall.ECONNABORTED, transport.ConnectionAborted},
		{"pipe", syscall.EPIPE, transport.BrokenPipe},
		{"eof", io.EOF, transport.EOF},
		{"unexpected_eof", io.ErrUnexpectedEOF, transport.EOF},
		{"tls_record", tls.RecordHeaderError{Msg: "private-tls-text"}, transport.TLS},
		{"tls_verification", &tls.CertificateVerificationError{Err: errors.New("private-cert")}, transport.TLS},
		{"tls_alert", tls.AlertError(42), transport.TLS},
		{"tls_authority", x509.UnknownAuthorityError{}, transport.TLS},
		{"tls_hostname", x509.HostnameError{Host: "private-host"}, transport.TLS},
		{"tls_invalid", x509.CertificateInvalidError{}, transport.TLS},
		{"tls_roots", x509.SystemRootsError{Err: errors.New("private-roots")}, transport.TLS},
		{"deadline", context.DeadlineExceeded, transport.Timeout},
		{"socket_timeout", syscall.ETIMEDOUT, transport.Timeout},
		{"canceled", context.Canceled, transport.Canceled},
		{"unknown", errors.New("tls DNS connection reset https://private-user:private-password@private-proxy"), transport.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err
			if err != nil {
				err = &url.Error{Op: "Post", URL: "https://private-user:private-password@private-host", Err: fmt.Errorf("private-context: %w", err)}
			}
			if got := transportReason(err); got != tc.want {
				t.Fatalf("reason=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestTransportReasonSurvivesHTTPHandlers(t *testing.T) {
	// Inject the failure at the real HTTP transport boundary. Host socket
	// behavior (RST vs refused) must not make this contract test flaky.
	previousTransport := http.DefaultTransport
	httpTransport := previousTransport.(*http.Transport).Clone()
	httpTransport.Proxy = nil
	httpTransport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	http.DefaultTransport = httpTransport
	previous := responsesURL
	endpoint := "http://example.test/responses"
	responsesURL = endpoint
	t.Cleanup(func() {
		responsesURL = previous
		http.DefaultTransport = previousTransport
		httpTransport.CloseIdleConnections()
	})
	up := &Upstream{}
	for _, kind := range []string{"start", "complete", "compact"} {
		t.Run(kind, func(t *testing.T) {
			body := []byte(`{"model":"gpt-test","input":[],"instructions":"test"}`)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r := event.NewResult("test", "test", up.ID())
			var attempt events.HTTPAttempt
			var class events.ErrorClass
			switch kind {
			case "start":
				up.handleStart(event.NewEventWithContext(events.TopicStart, "test", up.ID(), nil, ctx, events.StartCommand{AccessToken: "private-token", AccountIDHeader: "private-account", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.StartResult)
				attempt, class = got.Attempt, got.ErrorClass
			case "complete":
				up.handleComplete(event.NewEventWithContext(events.TopicComplete, "test", up.ID(), nil, ctx, events.CompleteCommand{AccessToken: "private-token", AccountIDHeader: "private-account", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.CompleteResult)
				attempt, class = got.Attempt, got.ErrorClass
			case "compact":
				up.handleCompact(event.NewEventWithContext(events.TopicCompact, "test", up.ID(), nil, ctx, events.CompactCommand{AccessToken: "private-token", AccountIDHeader: "private-account", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.CompactResult)
				attempt, class = got.Attempt, got.ErrorClass
			}
			if class != events.ErrorNetwork || attempt.Response.TransportReason != transport.ConnectionRefused || attempt.Response.Observed || attempt.Response.Status != 0 || attempt.Request.URL != endpoint {
				t.Fatalf("class=%s attempt=%+v", class, attempt)
			}
			encoded, err := json.Marshal(attempt)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private-") {
				t.Fatalf("unsafe observation: %s", encoded)
			}
		})
	}
}

func TestCompletedReadFailureKeepsTransportReason(t *testing.T) {
	previous := responsesURL
	t.Cleanup(func() { responsesURL = previous })
	up := &Upstream{}
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte(`{"object":"response"`)) // truncated body => unexpected EOF
		}))
		t.Cleanup(server.Close)
		responsesURL = server.URL
		for _, compact := range []bool{false, true} {
			r := event.NewResult("test", "test", up.ID())
			body := []byte(`{"model":"gpt-test","input":[],"instructions":"test"}`)
			var attempt events.HTTPAttempt
			var class events.ErrorClass
			if compact {
				up.handleCompact(event.NewEvent(events.TopicCompact, "test", up.ID(), nil, events.CompactCommand{AccessToken: "test", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.CompactResult)
				attempt, class = got.Attempt, got.ErrorClass
			} else {
				up.handleComplete(event.NewEvent(events.TopicComplete, "test", up.ID(), nil, events.CompleteCommand{AccessToken: "test", Body: body}), r)
				value, err := r.Get()
				if err != nil {
					t.Fatal(err)
				}
				got := value.(events.CompleteResult)
				attempt, class = got.Attempt, got.ErrorClass
			}
			if class != events.ErrorNetwork || attempt.Response.TransportReason != transport.EOF || !attempt.Response.Observed || attempt.Response.Status != 200 {
				t.Fatalf("compact=%t class=%s attempt=%+v", compact, class, attempt)
			}
		}
	}
}

type diagnosticErrorReader struct{ err error }

func (r diagnosticErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamReadReasonCrossesPullContract(t *testing.T) {
	for _, tc := range []struct {
		err    error
		class  events.ErrorClass
		reason transport.Reason
	}{
		{syscall.ECONNRESET, events.ErrorNetwork, transport.ConnectionReset},
		{io.EOF, events.ErrorProtocol, transport.EOF},
		{errors.New("private-proxy credentials"), events.ErrorNetwork, transport.Unknown},
	} {
		stream := &responseStream{cancel: func() {}, updates: make(chan streamUpdate, 8)}
		up := &Upstream{streams: map[string]*responseStream{"test": stream}}
		up.runStream(context.Background(), "test", stream, io.NopCloser(diagnosticErrorReader{tc.err}), 1024)
		r := event.NewResult(events.TopicPull, "test", up.ID())
		up.handlePull(event.NewEvent(events.TopicPull, "test", up.ID(), nil, events.PullCommand{StreamID: "test"}), r)
		value, err := r.Get()
		if err != nil {
			t.Fatal(err)
		}
		got := value.(events.PullResult)
		if !got.Done || got.ErrorClass != tc.class || got.TransportReason != tc.reason {
			t.Fatalf("pull=%+v", got)
		}
	}
	stream := &responseStream{updates: make(chan streamUpdate, 8)}
	(&Upstream{}).runStream(context.Background(), "test", stream, io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")), 4096)
	for update := range stream.updates {
		if update.errorClass != "" || update.transportReason != "" {
			t.Fatalf("normal terminal became failure: %+v", update)
		}
	}
}

func TestCompletedTransportReasonKeepsBusinessErrorsAbsent(t *testing.T) {
	for _, class := range []events.ErrorClass{events.ErrorProtocol, events.ErrorUpstream, events.ErrorInvalidRequest} {
		if got := completedTransportReason(class, errors.New("connection reset private-host")); got != "" {
			t.Fatalf("business error reason=%q", got)
		}
	}
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(": heartbeat\n\n"))}
	_, class, _, _, err := completedResponse(response, 1024)
	if class != events.ErrorProtocol || completedTransportReason(class, err) != transport.EOF {
		t.Fatalf("class=%s reason=%s", class, completedTransportReason(class, err))
	}
}

func TestErrorResponseBodyReadReasonPreservesHTTPFailure(t *testing.T) {
	response := &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"7"}}, Body: io.NopCloser(diagnosticErrorReader{syscall.ECONNRESET})}
	attempt := events.HTTPAttempt{Response: events.HTTPResponseObservation{Observed: true, Status: 503}}
	_, _, retry, _ := readArchivedErrorObservation(response, &attempt, codexRequestProfile{})
	if attempt.Response.TransportReason != transport.ConnectionReset || !attempt.Response.ErrorBodyReadFailed || attempt.Response.Status != 503 || !attempt.Response.Observed || retry != 7 || len(attempt.Response.ErrorBody) != 0 {
		t.Fatalf("observation=%+v retry=%d", attempt.Response, retry)
	}
}
