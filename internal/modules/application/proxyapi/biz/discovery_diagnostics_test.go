package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	codexcommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	codexupevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
	"github.com/muidea/magicCommon/event"
)

func TestDiscoveryLogsOriginalFailureWithStructuredDiagnostics(t *testing.T) {
	hub := event.NewHub(8)
	defer hub.Terminate(context.Background())
	chat := event.NewSimpleObserver(acccommon.UnitID, hub)
	chat.Subscribe(accevents.TopicRecordModelDiscoveryFailure, func(_ event.Event, result event.Result) {
		result.Set(accevents.RecordModelDiscoveryFailureResult{OK: true}, nil)
	})
	codex := event.NewSimpleObserver(codexcommon.UnitID, hub)
	codex.Subscribe(codexevents.TopicRecordModelDiscoveryFailure, func(_ event.Event, result event.Result) {
		result.Set(codexevents.RecordModelDiscoveryFailureResult{OK: true}, nil)
	})
	proxy := &Proxy{Base: basebiz.New("proxy-test", hub, nil)}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	cause := errors.New("original failure: http://original-user:original-password@original-proxy")
	proxy.recordDiscoveryFailure(context.Background(), accevents.DiscoveryCandidate{AccountID: "chat-account"}, "list_models", upevents.ErrClassUpstream, 403, "", cause)
	proxy.recordCodexDiscoveryFailure(context.Background(), codexevents.DiscoveryCandidate{AccountID: "codex-account"}, "list_models", codexupevents.ErrorNetwork, 0, transport.DNS, cause)
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logs=%s", logs.Bytes())
	}
	for i, line := range lines {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["error"] != cause.Error() || fields["stage"] != "list_models" {
			t.Fatalf("lost original diagnostics: %s", line)
		}
		if i == 0 && fields["upstream_status"] != float64(403) {
			t.Fatalf("lost status: %s", line)
		}
		if i == 1 && fields["transport_reason"] != "dns" {
			t.Fatalf("lost reason: %s", line)
		}
	}
}
