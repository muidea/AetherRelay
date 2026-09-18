package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CP-HDR-011: 客户端 turn metadata 被拆成"身份（不采用）/turn 级（采用）/属性（采用）"，
// 未知与越界字段只记 ignored-features，不让请求失败。
func TestCodexTurnMetadataProjectionSplitsOwnership(t *testing.T) {
	raw := `{
		"installation_id": "client-install",
		"session_id": "client-session",
		"thread_id": "client-thread",
		"window_id": "client-thread:0",
		"turn_id": "client-turn",
		"root_turn_id": "client-root-turn",
		"turn_started_at_unix_ms": 1789711880466,
		"window_number": 3,
		"request_kind": "turn",
		"sandbox": "none",
		"sandbox_mode": "danger-full-access",
		"agent_name": "/root",
		"auto_review_enabled": false,
		"node_repl_disabled": true,
		"unknown_future_key": "must-not-pass"
	}`
	projection, ignored := codexTurnMetadataProjection(raw)
	if projection.TurnID != "client-turn" || projection.RootTurnID != "client-root-turn" || projection.TurnStartedAtMS != 1789711880466 {
		t.Fatalf("CP-HDR-011 turn level=%+v", projection)
	}
	attributes := string(projection.Attributes)
	for _, want := range []string{`"window_number":3`, `"request_kind":"turn"`, `"sandbox_mode":"danger-full-access"`, `"agent_name":"/root"`, `"auto_review_enabled":false`, `"node_repl_disabled":true`} {
		if !strings.Contains(attributes, want) {
			t.Fatalf("CP-HDR-011 attributes lost %s: %s", want, attributes)
		}
	}
	// 身份字段与未知键都不转上游。
	for _, forbidden := range []string{"client-install", "client-session", "client-thread", "unknown_future_key"} {
		if strings.Contains(attributes, forbidden) {
			t.Fatalf("CP-HDR-011 leaked %s: %s", forbidden, attributes)
		}
	}
	joined := strings.Join(ignored, ",")
	for _, want := range []string{"turn_metadata.session_id", "turn_metadata.thread_id", "turn_metadata.window_id", "turn_metadata.installation_id", "turn_metadata.unknown_future_key"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("CP-HDR-011 ignored features missing %s: %v", want, ignored)
		}
	}
	// 非标量属性被拒绝，但请求本身不受影响。
	scalarOnly, scalarIgnored := codexTurnMetadataProjection(`{"sandbox":{"nested":true},"request_kind":"turn"}`)
	if strings.Contains(string(scalarOnly.Attributes), "nested") || !strings.Contains(strings.Join(scalarIgnored, ","), "turn_metadata.sandbox") {
		t.Fatalf("CP-HDR-011 non-scalar attribute=%s ignored=%v", scalarOnly.Attributes, scalarIgnored)
	}
}

// CP-HDR-011: 头部优先，缺失时回落到 body client_metadata 内嵌的同名 JSON。
func TestCodexTurnMetadataSourcePrefersHeader(t *testing.T) {
	body := []byte(`{"model":"gpt-test","client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"from-body\"}"}}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if got := codexTurnMetadataSource(request.Header, body); !strings.Contains(got, "from-body") {
		t.Fatalf("CP-HDR-011 body fallback=%q", got)
	}
	request.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"from-header"}`)
	if got := codexTurnMetadataSource(request.Header, body); !strings.Contains(got, "from-header") {
		t.Fatalf("CP-HDR-011 header precedence=%q", got)
	}
}

// CP-HDR-011: 越界或非法输入不转上游，也不产生错误；无输入时投影为空。
func TestCodexTurnMetadataProjectionStaysBounded(t *testing.T) {
	if projection, ignored := codexTurnMetadataProjection(""); projection.TurnID != "" || len(ignored) != 0 {
		t.Fatalf("CP-HDR-011 empty projection=%+v ignored=%v", projection, ignored)
	}
	if _, ignored := codexTurnMetadataProjection("not json"); len(ignored) == 0 {
		t.Fatal("CP-HDR-011 malformed metadata was not reported")
	}
	if _, ignored := codexTurnMetadataProjection(strings.Repeat("x", codexTurnMetadataLimit+1)); len(ignored) == 0 {
		t.Fatal("CP-HDR-011 oversized metadata was not reported")
	}
	// 超长单值与超量属性只丢字段。
	oversized := `{"agent_name":"` + strings.Repeat("y", codexTurnMetadataValueLimit+1) + `","request_kind":"turn"}`
	projection, ignored := codexTurnMetadataProjection(oversized)
	if strings.Contains(string(projection.Attributes), "yyy") || !strings.Contains(strings.Join(ignored, ","), "turn_metadata.agent_name") {
		t.Fatalf("CP-HDR-011 oversized value=%s ignored=%v", projection.Attributes, ignored)
	}
}
