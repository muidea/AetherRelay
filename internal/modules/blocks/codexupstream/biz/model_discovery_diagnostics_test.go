package biz

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	"github.com/muidea/magicCommon/event"
)

func TestModelDiscoveryHandlerKeepsHTTPAndTransportDiagnostics(t *testing.T) {
	previousURL, previousTransport := modelsURL, http.DefaultTransport
	t.Cleanup(func() { modelsURL, http.DefaultTransport = previousURL, previousTransport })
	up := &Upstream{}
	for _, status := range []int{403, 429, 200} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("original-models-body"))
		}))
		modelsURL = server.URL
		result := event.NewResult(events.TopicListModels, "test", "upstream")
		up.handleListModels(event.NewEventWithContext(events.TopicListModels, "test", "upstream", nil, context.Background(), events.ListModelsCommand{AccessToken: "test-token"}), result)
		server.Close()
		value, err := result.Get()
		got, ok := value.(events.ListModelsResult)
		if !ok || err == nil || got.HTTPStatus != status || got.TransportReason != "" || got.ErrorClass == "" {
			t.Fatalf("got=%+v err=%v", value, err)
		}
	}
	tr := previousTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	defer tr.CloseIdleConnections()
	http.DefaultTransport = tr
	modelsURL = "http://original-upstream.test/models"
	result := event.NewResult(events.TopicListModels, "test", "upstream")
	up.handleListModels(event.NewEventWithContext(events.TopicListModels, "test", "upstream", nil, context.Background(), events.ListModelsCommand{AccessToken: "test-token"}), result)
	value, err := result.Get()
	got := value.(events.ListModelsResult)
	if err == nil || got.HTTPStatus != 0 || got.TransportReason != transport.ConnectionRefused || !strings.Contains(err.Error(), "original-upstream.test") {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
