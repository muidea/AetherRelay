package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"aetherrelay/internal/pkg/aetherrelaystate"
)

// Both pools share a database. Even colliding record IDs must remain isolated
// when Web startup migrates a device, records a limit, or deletes an account.
func TestWebDeviceMigrationAndFeedbackPreserveCodexDocuments(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "legacy_missing_device"
		if malformed {
			name = "invalid_device_isolated_from_startup"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.duckdb")
			codec := encryptedTestCodec(t)
			documents, err := aetherrelaystate.Open(path, "128MB", 1)
			if err != nil {
				t.Fatal(err)
			}
			defer documents.Close()
			const id, codexScope = "same-account-id", "codex_oauth_accounts"
			web := map[string]any{"id": id, "access_token": "web-token", "email": "same@example.com", "status": StatusNormal, "source_type": "web", "created_at": "2026-10-28T00:00:00Z"}
			if malformed {
				web["web_device_id"] = "malformed-device"
			}
			webPayload, err := json.Marshal(web)
			if err != nil {
				t.Fatal(err)
			}
			codexPayload := []byte(`{"id":"same-account-id","access_token":"codex-token","refresh_token":"codex-refresh","email":"same@example.com","max_concurrency":6,"cooldowns":{"gpt-6-sol":{"until":"2026-10-29T00:00:00Z","error_class":"rate_limit"}},"fingerprint_seed":"codex-seed"}`)
			for scope, payload := range map[string][]byte{secureDocumentScope: webPayload, codexScope: codexPayload} {
				sealed, err := codec.Seal(scope, id, payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := documents.ApplySecureDocuments(scope, []aetherrelaystate.SecureDocumentRow{{ID: id, Position: 7, Payload: sealed}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			before, err := documents.LoadSecureDocuments(codexScope)
			if err != nil || len(before) != 1 {
				t.Fatalf("seed Codex: %v, rows=%d", err, len(before))
			}
			pool, err := Open(path, "128MB", 1, 6, codec)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if malformed {
				view, ok := pool.ViewForAccessToken("web-token")
				if !ok || view.Status != StatusAbnormal || view.DeviceIdentityError != "invalid_device_identity" || view.DeviceID != "malformed-device" {
					t.Fatalf("malformed identity not isolated: %+v", view)
				}
				encoded, err := json.Marshal(view)
				if err != nil || strings.Contains(string(encoded), "malformed-device") {
					t.Fatal("management JSON exposed stored device value")
				}
			} else {
				view, ok := pool.ViewForAccessToken("web-token")
				if !ok {
					t.Fatal("Web migration lost account")
				}
				requireDevice(t, view.DeviceID)
			}
			if _, ok := pool.RecordTextResult(id, "gpt-6-sol", false, "rate_limit"); !ok {
				t.Fatal("Web feedback lost account")
			}
			if deleted, err := pool.DeleteByIDs([]string{id}); err != nil || deleted != 1 {
				t.Fatalf("Web deletion: deleted=%d, err=%v", deleted, err)
			}
			after, err := documents.LoadSecureDocuments(codexScope)
			if err != nil || len(after) != 1 {
				t.Fatalf("Codex document lost: %v, rows=%d", err, len(after))
			}
			if before[0].ID != after[0].ID || before[0].Position != after[0].Position || !before[0].UpdatedAt.Equal(after[0].UpdatedAt) || !bytes.Equal(before[0].Payload, after[0].Payload) {
				t.Fatal("Web operation modified Codex encrypted document or revision")
			}
		})
	}
}
