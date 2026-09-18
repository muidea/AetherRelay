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
	projection, ignored := codexTurnMetadataProjection(nil, raw)
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
	scalarOnly, scalarIgnored := codexTurnMetadataProjection(nil, `{"sandbox":{"nested":true},"request_kind":"turn"}`)
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
	if projection, ignored := codexTurnMetadataProjection(nil, ""); projection.TurnID != "" || len(ignored) != 0 {
		t.Fatalf("CP-HDR-011 empty projection=%+v ignored=%v", projection, ignored)
	}
	if _, ignored := codexTurnMetadataProjection(nil, "not json"); len(ignored) == 0 {
		t.Fatal("CP-HDR-011 malformed metadata was not reported")
	}
	if _, ignored := codexTurnMetadataProjection(nil, strings.Repeat("x", codexTurnMetadataLimit+1)); len(ignored) == 0 {
		t.Fatal("CP-HDR-011 oversized metadata was not reported")
	}
	// 超长单值与超量属性只丢字段。
	oversized := `{"agent_name":"` + strings.Repeat("y", codexTurnMetadataValueLimit+1) + `","request_kind":"turn"}`
	projection, ignored := codexTurnMetadataProjection(nil, oversized)
	if strings.Contains(string(projection.Attributes), "yyy") || !strings.Contains(strings.Join(ignored, ","), "turn_metadata.agent_name") {
		t.Fatalf("CP-HDR-011 oversized value=%s ignored=%v", projection.Attributes, ignored)
	}
}

// CP-HDR-010: 窗口号取客户端声明，header 优先、属性兜底；不可解析时不猜。
func TestCodexTurnMetadataWindowNumberResolution(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Window-Id", "01a080cb-abf8-7900-97a7-7af78ed32b94:37")
	projection, _ := codexTurnMetadataProjection(headers, `{"window_number":12}`)
	if projection.WindowNumber != 37 || !strings.Contains(string(projection.Attributes), `"window_number":37`) {
		t.Fatalf("CP-HDR-010 header wins: %+v", projection)
	}
	// 显式 :0 也是有效 header 声明，不能被属性的非零值当成“未声明”覆盖。
	headers.Set("X-Codex-Window-Id", "01a080cb-abf8-7900-97a7-7af78ed32b94:0")
	projection, _ = codexTurnMetadataProjection(headers, `{"window_number":12}`)
	if projection.WindowNumber != 0 || !strings.Contains(string(projection.Attributes), `"window_number":0`) {
		t.Fatalf("CP-HDR-010 explicit zero lost precedence: %+v", projection)
	}
	// 头部缺失时用同一份元数据里的 window_number。
	projection, _ = codexTurnMetadataProjection(nil, `{"window_number":12}`)
	if projection.WindowNumber != 12 {
		t.Fatalf("CP-HDR-010 attribute fallback: %+v", projection)
	}
	// 头部存在但号段不可用时仍回落到属性。
	headers.Set("X-Codex-Window-Id", "01a080cb-abf8-7900-97a7-7af78ed32b94:next")
	projection, _ = codexTurnMetadataProjection(headers, `{"window_number":12}`)
	if projection.WindowNumber != 12 {
		t.Fatalf("CP-HDR-010 unusable header: %+v", projection)
	}
	// 元数据整体不可解析时，header 仍然是有效来源。
	headers.Set("X-Codex-Window-Id", "01a080cb-abf8-7900-97a7-7af78ed32b94:37")
	projection, ignored := codexTurnMetadataProjection(headers, "not json")
	if projection.WindowNumber != 37 || len(ignored) == 0 {
		t.Fatalf("CP-HDR-010 header without metadata=%+v ignored=%v", projection, ignored)
	}
	// 无声明、越界与负数都不猜测，落回 0。
	for _, value := range []string{"", "01a080cb-abf8-7900-97a7-7af78ed32b94", "01a080cb-abf8-7900-97a7-7af78ed32b94:", "01a080cb-abf8-7900-97a7-7af78ed32b94:-1"} {
		headers.Set("X-Codex-Window-Id", value)
		projection, ignored = codexTurnMetadataProjection(headers, `{"window_number":-2}`)
		if projection.WindowNumber != 0 || strings.Contains(string(projection.Attributes), "window_number") || !strings.Contains(strings.Join(ignored, ","), "turn_metadata.window_number") {
			t.Fatalf("CP-HDR-010 unexpected window number for %q: %+v ignored=%v", value, projection, ignored)
		}
	}
}
