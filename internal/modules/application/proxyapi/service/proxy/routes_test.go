package proxy

import (
	"net/http"
	"testing"

	enginehttp "github.com/muidea/magicEngine/http"
)

func TestRegisterRoutesIncludesDedicatedSearchEndpoint(t *testing.T) {
	routes := enginehttp.NewRouteRegistry()
	RegisterRoutes(routes, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if !routes.ExistHandler("/v1/search", http.MethodPost) {
		t.Fatal("POST /v1/search was not registered")
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
