package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginBackgroundWithoutSession(t *testing.T) {
	h := newAuthHandler(t, enabledAuthConfig(t, "ops-admin", "s3cret-pass"))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/ops/AetherRelay/assets/login-background.webp", nil)
			req.RemoteAddr = "203.0.113.8:9"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
				t.Fatalf("background status=%d headers=%v", rec.Code, rec.Header())
			}
			for key, want := range map[string]string{"Content-Type": "image/webp", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
				if got := rec.Header().Get(key); got != want {
					t.Fatalf("%s=%q, want %q", key, got, want)
				}
			}
			if method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Fatal("HEAD returned image bytes")
				}
				return
			}
			data := rec.Body.Bytes()
			if len(data) < 12 || !bytes.HasPrefix(data, []byte("RIFF")) || string(data[8:12]) != "WEBP" {
				t.Fatal("background is not a WebP image")
			}
			if len(data) > 100*1024 {
				t.Fatalf("login background exceeds 100 KiB budget: %d bytes", len(data))
			}
		})
	}
	// Only the exact artwork is public; other assets remain protected.
	req := httptest.NewRequest(http.MethodGet, "/ops/AetherRelay/assets/login-background.prompt.md", nil)
	req.RemoteAddr = "203.0.113.8:9"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unexpected public asset access: %d", rec.Code)
	}
}

func TestLoginBackgroundPageUsesCustomBaseAndAccessibleFallback(t *testing.T) {
	page := string(loginPageHTML("/ops/AetherRelay", "en-US"))
	for _, marker := range []string{
		`window.__AETHERRELAY_ADMIN_BASE_PATH__="/ops/AetherRelay"`,
		`base+"/assets/login-background.webp"`,
		`class="login-background" alt="" aria-hidden="true"`,
		`object-fit:cover`, `object-position:center`, `pointer-events:none`,
		`min-height:100svh`, `background:radial-gradient`,
		`--bg:#edf5fc`, `ellipse at center,#fff,var(--bg)`,
		`autocomplete="username"`, `autocomplete="current-password"`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("missing login background marker %q", marker)
		}
	}
}
