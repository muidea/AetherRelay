package admin

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticationLogIdentifiesRouteAndAuthType(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	r := httptest.NewRequest(http.MethodGet, "/admin/api/usage/events?token=private-query", nil)
	r.Header.Set("Cookie", "private-cookie")
	w := httptest.NewRecorder()
	writeAdminAuthError(w, r, http.StatusUnauthorized, "admin_authentication_required", "admin login is required")
	text := logs.String()
	for _, want := range []string{"authentication rejected", "/admin/api/usage/events", "admin_session", "admin_authentication_required"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "private-") {
		t.Fatal("request credentials entered auth logs", text)
	}
}
