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
