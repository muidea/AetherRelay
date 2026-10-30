package biz

import (
	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"testing"
)

func TestCodexOverloadTerminalPreservesStructuredError(t *testing.T) {
	for _, payload := range []string{
		`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","param":null,"message":"Our servers are currently overloaded. Please try again later."}}`,
		`{"type":"response.failed","response":{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","param":null,"message":"Our servers are currently overloaded. Please try again later."}}}`,
	} {
		class, quota, diagnostic := codexTerminalFailure([]byte(payload))
		if class != events.ErrorUpstream || quota.UsageLimited || diagnostic.Code != "server_is_overloaded" || diagnostic.Type != "service_unavailable_error" || diagnostic.Param != "" || diagnostic.Message != "Our servers are currently overloaded. Please try again later." {
			t.Fatalf("class=%s quota=%+v diagnostic=%+v", class, quota, diagnostic)
		}
	}
}
