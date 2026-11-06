package biz

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
)

type watchdogLogWriter struct {
	bytes.Buffer
	waiting chan struct{}
}

func (w *watchdogLogWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte("Codex account command still waiting")) {
		select {
		case w.waiting <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestAccountAdmissionFailureLogsReasonWithoutRawError(t *testing.T) {
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, r event.Result) {
		r.Set(accevents.AcquireResult{UnavailableReason: "accounts_busy"}, cd.NewError(cd.Unexpected, "secret-token-and-proxy"))
	})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	s := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, nil)}
	ctx := codexresponses.WithExecutionIdentity(context.Background(), codexresponses.ExecutionIdentity{RequestID: "request", EventID: "event", APIKeyID: "key-id", Endpoint: "/v1/responses"})
	_, err := s.acquireCodexAccount(ctx, "gpt-test", nil)
	if err == nil {
		t.Fatal("admission succeeded")
	}
	for _, field := range []string{`"stage":"acquire"`, `"request_id":"request"`, `"event_id":"event"`, `"unavailable_reason":"accounts_busy"`} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing %s: %s", field, logs.String())
		}
	}
	if strings.Contains(logs.String(), "secret-token-and-proxy") {
		t.Fatal("raw command error leaked")
	}
}

func TestAccountCommandReportsOutstandingWaitAndRecovery(t *testing.T) {
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	entered, unblock := make(chan struct{}), make(chan struct{})
	accounts.Subscribe(accevents.TopicPutUsageSnapshot, func(_ event.Event, r event.Result) {
		close(entered)
		<-unblock
		r.Set(accevents.PutUsageSnapshotResult{OK: true}, nil)
	})
	// slog's handler serializes writes. Read the buffer only after the
	// command and its diagnostic timer have both finished.
	logs := watchdogLogWriter{waiting: make(chan struct{}, 1)}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	s := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, nil)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.sendCodexAccountCommand(event.NewEvent(accevents.TopicPutUsageSnapshot, s.ID(), acccommon.UnitID, nil, nil), "persist_usage_snapshot", "slot")
	}()
	<-entered
	select {
	case <-logs.waiting:
	case <-time.After(codexAccountCommandSlow + time.Second):
		close(unblock)
		<-done
		t.Fatal("outstanding command was not reported")
	}
	close(unblock)
	<-done
	for _, message := range []string{"Codex account command still waiting", "Codex account command completed", `"reason":"completed"`, `"account_id":"slot"`} {
		if !strings.Contains(logs.String(), message) {
			t.Fatalf("missing %s: %s", message, logs.String())
		}
	}
}
