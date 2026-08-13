package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	enginehttp "github.com/muidea/magicEngine/http"
)

func TestRegisterRoutesIncludesDedicatedSearchEndpoint(t *testing.T) {
	routes := enginehttp.NewRouteRegistry()
	RegisterRoutes(routes, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if !routes.ExistHandler("/v1/search", http.MethodPost) {
		t.Fatal("POST /v1/search was not registered")
	}
}

func TestRouteRegistryPreservesWebsocketHijacker(t *testing.T) {
	routes := enginehttp.NewRouteRegistry()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	routes.AddHandler("/v1/responses", http.MethodGet, func(_ context.Context, w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade through RouteRegistry: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ready"))
	})
	server := enginehttp.NewHTTPServer()
	server.Bind(routes)
	httpServer := httptest.NewServer(server.(http.Handler))
	t.Cleanup(httpServer.Close)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("CP-WS-001 production route handshake: %v", err)
	}
	defer conn.Close()
	_, payload, err := conn.ReadMessage()
	if err != nil || string(payload) != "ready" {
		t.Fatalf("websocket payload=%q err=%v", payload, err)
	}
}

func TestRegisterRoutesExcludesRetiredCodexCompatibilityEndpoints(t *testing.T) {
	routes := enginehttp.NewRouteRegistry()
	RegisterRoutes(routes, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/responses/ws"},
		{method: http.MethodGet, path: "/backend-api/codex/responses"},
		{method: http.MethodPost, path: "/backend-api/codex/responses"},
		{method: http.MethodPost, path: "/backend-api/codex/responses/compact"},
		{method: http.MethodGet, path: "/backend-api/codex/models"},
		{method: http.MethodPost, path: "/backend-api/codex/models"},
	} {
		if routes.ExistHandler(testCase.path, testCase.method) {
			t.Fatalf("CP-EP-004..006/CP-EP-012 retired endpoint was registered: %s %s", testCase.method, testCase.path)
		}
	}
}
