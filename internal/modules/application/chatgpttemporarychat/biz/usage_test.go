package biz

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	tempcommon "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	configcommon "ai-proxy/internal/modules/blocks/configruntime/pkg/common"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	usagecommon "ai-proxy/internal/modules/blocks/usageruntime/pkg/common"
	usageevents "ai-proxy/internal/modules/blocks/usageruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestUsageOutcomeFromTurn(t *testing.T) {
	cases := []struct {
		class                  string
		cancelled, interrupted bool
		wantOutcome, wantCode  string
	}{
		{"", false, false, "success", ""},
		{"cancelled", true, false, "client_canceled", "client_canceled"},
		{"interrupted", false, true, "process_interrupted", "process_interrupted"},
		{"rate_limit", false, false, "upstream_failed", "rate_limit"},
		{"invalid_token", false, false, "upstream_failed", "invalid_token"},
		{"weird", false, false, "upstream_failed", "upstream"},
	}
	for _, tc := range cases {
		outcome, code := usageOutcomeFromTurn(tc.class, tc.cancelled, tc.interrupted)
		if outcome != tc.wantOutcome || code != tc.wantCode {
			t.Fatalf("%+v -> %s/%s", tc, outcome, code)
		}
	}
}

func TestCompleteTurnUsageWritesEstimatedAdminEvent(t *testing.T) {
	mem := usage.NewMemoryStore()
	s := &TemporaryChat{usage: mem}
	started := time.Now().UTC().Add(-time.Second)
	if err := mem.Start(context.Background(), usage.StartRecord{
		EventID: "evt-1", StartedAt: started, APIKeyID: "admin:ops", Provider: "chatgptweb", Model: "gpt-5",
	}); err != nil {
		t.Fatal(err)
	}
	s.completeTurnUsage("evt-1", "gpt-5", "gpt-5-actual", "ops", "be brief", "hello", true, "world", httpStatusAccepted, "success", "", started, true)
	page, err := mem.Events(context.Background(), usage.EventFilter{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events=%d", len(page.Events))
	}
	ev := page.Events[0]
	if ev.EventID != "evt-1" || ev.Provider != "chatgptweb" || ev.Model != "gpt-5-actual" {
		t.Fatalf("event=%+v", ev)
	}
	if ev.APIKeyID != "admin:ops" {
		t.Fatalf("identity fields=%+v", ev)
	}
	if !ev.Estimated || ev.Outcome != "success" || ev.HTTPStatus != 202 || !ev.Stream {
		t.Fatalf("event=%+v", ev)
	}
	if ev.UpstreamEndpoint != "chatgptweb_temporary_chat" || ev.UpstreamProtocol != "chatgptweb" {
		t.Fatalf("upstream fields=%+v", ev)
	}
	if ev.InputTokens <= 0 || ev.OutputTokens <= 0 {
		t.Fatalf("expected estimated tokens: %+v", ev)
	}
}

func wireBootstrap(hub event.Hub, t *testing.T) (dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "state.duckdb")
	stateDir := t.TempDir()
	cfgObs := event.NewSimpleObserver(configcommon.UnitID, hub)
	cfgObs.Subscribe(configevents.TopicBootstrap, func(_ event.Event, result event.Result) {
		result.Set(configevents.BootstrapResult{Bootstrap: configevents.Bootstrap{Config: config.Config{
			ChatGPTWeb: config.ChatGPTWebConfig{
				Enabled: true,
				TemporaryChat: config.TemporaryChatConfig{
					Enabled: true, RetentionDays: 30, MaxConversations: 20,
					MaxMessagesPerConversation: 50, MaxMessageBytes: 8192, TurnTimeoutSeconds: 30,
				},
			},
			State: config.StateConfig{Dir: stateDir, Database: dbPath, MemoryLimit: "64MB", Threads: 1},
		}}}, nil)
	})
	return dbPath
}

func TestStartTurnUsageFailureDoesNotCallUpstream(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	_ = wireBootstrap(hub, t)

	usageObs := event.NewSimpleObserver(usagecommon.UnitID, hub)
	usageObs.Subscribe(usageevents.TopicAcquire, func(_ event.Event, result event.Result) {
		result.Set(usageevents.AcquireResult{}, nil)
	})
	startCalls := 0
	usageObs.Subscribe(usageevents.TopicStart, func(_ event.Event, result event.Result) {
		startCalls++
		result.Set(nil, cd.NewError(cd.Unexpected, "usage start failed"))
	})

	upstreamCalls := 0
	upObs := event.NewSimpleObserver(upcommon.UnitID, hub)
	upObs.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
		upstreamCalls++
		result.Set(upevents.StartTextResult{StreamID: "should-not-happen"}, nil)
	})

	accObs := event.NewSimpleObserver(acccommon.UnitID, hub)
	accObs.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		t.Fatal("account acquire must not run when usage.Start fails")
		result.Set(accevents.AcquireTextTokenResult{}, nil)
	})
	accObs.Subscribe(accevents.TopicAcquireTextAccount, func(_ event.Event, result event.Result) {
		t.Fatal("account acquire must not run when usage.Start fails")
		result.Set(accevents.AcquireTextAccountResult{}, nil)
	})

	tc, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tc.store == nil || tc.usage == nil {
		t.Fatal("expected store and usage to be wired")
	}
	defer tc.Teardown(context.Background())

	view, cerr := tc.store.CreateConversation("ops", "gpt-5", "", "", "account-1")
	if cerr != nil {
		t.Fatal(cerr)
	}

	res := tc.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{
		OwnerID: "ops", ConversationID: view.ID, Content: "hello",
	}))
	_, getErr := res.Get()
	if getErr == nil {
		t.Fatal("expected usage start failure")
	}
	if startCalls != 1 {
		t.Fatalf("usage start calls=%d", startCalls)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream must not be called, calls=%d", upstreamCalls)
	}
	detail, derr := tc.store.GetConversation("ops", view.ID, nil, 20)
	if derr != nil {
		t.Fatal(derr)
	}
	if detail.Conversation.Status == "streaming" {
		t.Fatalf("status should not stay streaming after usage failure: %+v", detail.Conversation)
	}
}

func TestStartTurnAccountUnavailableCompletesUsageBeforeReturning(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)
	_ = wireBootstrap(hub, t)

	completed := make(chan usage.CompleteRecord, 1)
	usageObs := event.NewSimpleObserver(usagecommon.UnitID, hub)
	usageObs.Subscribe(usageevents.TopicAcquire, func(_ event.Event, result event.Result) { result.Set(usageevents.AcquireResult{}, nil) })
	usageObs.Subscribe(usageevents.TopicStart, func(_ event.Event, result event.Result) { result.Set(nil, nil) })
	usageObs.Subscribe(usageevents.TopicComplete, func(ev event.Event, result event.Result) {
		completed <- ev.Data().(usageevents.CompleteCommand).Record
		result.Set(nil, nil)
	})
	accountObs := event.NewSimpleObserver(acccommon.UnitID, hub)
	accountObs.Subscribe(accevents.TopicAcquireTextAccount, func(_ event.Event, result event.Result) {
		result.Set(nil, cd.NewError(cd.Unexpected, "account unavailable"))
	})

	tc, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tc.Teardown(context.Background())
	view, createErr := tc.store.CreateConversation("ops", "gpt-5", "", "system", "account-1")
	if createErr != nil {
		t.Fatal(createErr)
	}
	_, startErr := tc.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{OwnerID: "ops", ConversationID: view.ID, Content: "hello"})).Get()
	if startErr == nil {
		t.Fatal("expected account-unavailable start error")
	}
	select {
	case rec := <-completed:
		if rec.HTTPStatus != httpStatusServiceUnavailable || rec.Outcome != "upstream_failed" || rec.ErrorCode != "provider_unavailable" || !rec.Stream {
			t.Fatalf("usage completion=%+v", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("missing usage completion")
	}
}

func TestStartTurnUpstreamStartFailureCompletesUsageBeforeReturning(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)
	_ = wireBootstrap(hub, t)

	completed := make(chan usage.CompleteRecord, 1)
	usageObs := event.NewSimpleObserver(usagecommon.UnitID, hub)
	usageObs.Subscribe(usageevents.TopicAcquire, func(_ event.Event, result event.Result) { result.Set(usageevents.AcquireResult{}, nil) })
	usageObs.Subscribe(usageevents.TopicStart, func(_ event.Event, result event.Result) { result.Set(nil, nil) })
	usageObs.Subscribe(usageevents.TopicComplete, func(ev event.Event, result event.Result) {
		completed <- ev.Data().(usageevents.CompleteCommand).Record
		result.Set(nil, nil)
	})
	accountObs := event.NewSimpleObserver(acccommon.UnitID, hub)
	accountObs.Subscribe(accevents.TopicAcquireTextAccount, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextAccountResult{AccessToken: "tok", Account: accevents.AccountView{ID: "account-1"}}, nil)
	})
	accountObs.Subscribe(accevents.TopicRecordTextResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordTextResultResult{}, nil) })
	upstreamObs := event.NewSimpleObserver(upcommon.UnitID, hub)
	upstreamObs.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
		result.Set(nil, cd.NewError(cd.Unexpected, "upstream unavailable"))
	})

	tc, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tc.Teardown(context.Background())
	view, createErr := tc.store.CreateConversation("ops", "gpt-5", "", "", "account-1")
	if createErr != nil {
		t.Fatal(createErr)
	}
	_, startErr := tc.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{OwnerID: "ops", ConversationID: view.ID, Content: "hello"})).Get()
	if startErr == nil {
		t.Fatal("expected upstream start error")
	}
	select {
	case rec := <-completed:
		if rec.HTTPStatus != httpStatusBadGateway || rec.Outcome != "upstream_failed" || rec.ErrorCode != "upstream" || !rec.Stream {
			t.Fatalf("usage completion=%+v", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("missing usage completion")
	}
}

func TestStartTurnSuccessStartsUsageThenWorkerCompletes(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)

	_ = wireBootstrap(hub, t)

	var recordsMu sync.Mutex
	var started usage.StartRecord
	completedCh := make(chan usage.CompleteRecord, 1)
	usageObs := event.NewSimpleObserver(usagecommon.UnitID, hub)
	usageObs.Subscribe(usageevents.TopicAcquire, func(_ event.Event, result event.Result) {
		result.Set(usageevents.AcquireResult{}, nil)
	})
	usageObs.Subscribe(usageevents.TopicStart, func(ev event.Event, result event.Result) {
		cmd := ev.Data().(usageevents.StartCommand)
		recordsMu.Lock()
		started = cmd.Record
		recordsMu.Unlock()
		result.Set(nil, nil)
	})
	usageObs.Subscribe(usageevents.TopicComplete, func(ev event.Event, result event.Result) {
		cmd := ev.Data().(usageevents.CompleteCommand)
		completedCh <- cmd.Record
		result.Set(nil, nil)
	})

	accObs := event.NewSimpleObserver(acccommon.UnitID, hub)
	accObs.Subscribe(accevents.TopicAcquireTextAccount, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextAccountResult{
			AccessToken: "tok",
			Account:     accevents.AccountView{ID: "account-1", Status: "正常"},
		}, nil)
	})
	accObs.Subscribe(accevents.TopicAcquireTextToken, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextTokenResult{
			AccessToken: "tok",
			Account:     accevents.AccountView{ID: "account-1", Status: "正常"},
		}, nil)
	})
	accObs.Subscribe(accevents.TopicRecordTextResult, func(_ event.Event, result event.Result) {
		result.Set(accevents.RecordTextResultResult{}, nil)
	})

	upObs := event.NewSimpleObserver(upcommon.UnitID, hub)
	upObs.Subscribe(upevents.TopicStartText, func(_ event.Event, result event.Result) {
		result.Set(upevents.StartTextResult{StreamID: "stream-1"}, nil)
	})
	pulls := 0
	upObs.Subscribe(upevents.TopicPullText, func(_ event.Event, result event.Result) {
		pulls++
		if pulls == 1 {
			result.Set(upevents.PullTextResult{Delta: "hi ", ActualModel: "gpt-5-actual"}, nil)
			return
		}
		result.Set(upevents.PullTextResult{
			Delta: "there", Done: true, ConversationID: "up-1", AssistantMessageID: "as-1", ActualModel: "gpt-5-actual",
		}, nil)
	})
	upObs.Subscribe(upevents.TopicCancelText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CancelTextResult{Cancelled: true}, nil)
	})

	tc, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tc.Teardown(context.Background())

	view, cerr := tc.store.CreateConversation("ops", "gpt-5", "", "sys", "account-1")
	if cerr != nil {
		t.Fatal(cerr)
	}
	res := tc.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{
		OwnerID: "ops", ConversationID: view.ID, Content: "hello",
	}))
	value, getErr := res.Get()
	if getErr != nil {
		t.Fatalf("StartTurn: %v", getErr)
	}
	startRes, ok := value.(events.StartTurnResult)
	if !ok || startRes.TurnID == "" {
		t.Fatalf("start result=%T %+v", value, value)
	}
	recordsMu.Lock()
	startedCopy := started
	recordsMu.Unlock()
	if startedCopy.EventID == "" || startedCopy.APIKeyID != "admin:ops" || startedCopy.Provider != "chatgptweb" {
		t.Fatalf("usage start=%+v", startedCopy)
	}

	var completed usage.CompleteRecord
	select {
	case completed = <-completedCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("usage complete missing start=%+v", startedCopy)
	}
	if completed.EventID == "" || completed.EventID != startedCopy.EventID {
		t.Fatalf("usage complete mismatch start=%+v complete=%+v", startedCopy, completed)
	}
	if completed.Outcome != "success" || completed.HTTPStatus != 202 || !completed.Estimated {
		t.Fatalf("complete=%+v", completed)
	}
	if completed.Model != "gpt-5-actual" || completed.UpstreamEndpoint != "chatgptweb_temporary_chat" {
		t.Fatalf("complete=%+v", completed)
	}
	if completed.InputTokens <= 0 || completed.OutputTokens <= 0 {
		t.Fatalf("tokens=%+v", completed)
	}
}

func TestTemporaryTurnRefreshesInvalidTokenBeforeFirstDelta(t *testing.T) {
	hub := event.NewHub(16)
	background := task.NewBackgroundRoutine(8)
	defer hub.Terminate(context.Background())
	defer background.Shutdown(nil)
	_ = wireBootstrap(hub, t)

	usageObs := event.NewSimpleObserver(usagecommon.UnitID, hub)
	usageObs.Subscribe(usageevents.TopicAcquire, func(_ event.Event, result event.Result) { result.Set(usageevents.AcquireResult{}, nil) })
	usageObs.Subscribe(usageevents.TopicStart, func(_ event.Event, result event.Result) { result.Set(nil, nil) })
	usageObs.Subscribe(usageevents.TopicComplete, func(_ event.Event, result event.Result) { result.Set(nil, nil) })

	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	accounts.Subscribe(accevents.TopicAcquireTextAccount, func(_ event.Event, result event.Result) {
		result.Set(accevents.AcquireTextAccountResult{
			AccessToken: "old-token",
			Account:     accevents.AccountView{ID: "account-1", Proxy: "http://old-proxy.invalid:8080"},
		}, nil)
	})
	accounts.Subscribe(accevents.TopicRefreshTextToken, func(ev event.Event, result event.Result) {
		if command := ev.Data().(accevents.RefreshTextTokenCommand); command.AccessToken != "old-token" {
			t.Fatalf("refresh command=%+v", command)
		}
		result.Set(accevents.RefreshTextTokenResult{
			AccessToken: "new-token",
			Account:     accevents.AccountView{ID: "account-1", Proxy: "http://new-proxy.invalid:8080"},
			Refreshed:   true,
		}, nil)
	})
	recorded := make(chan accevents.RecordTextResultCommand, 1)
	accounts.Subscribe(accevents.TopicRecordTextResult, func(ev event.Event, result event.Result) {
		recorded <- ev.Data().(accevents.RecordTextResultCommand)
		result.Set(accevents.RecordTextResultResult{}, nil)
	})

	upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
	starts := 0
	upstream.Subscribe(upevents.TopicStartText, func(ev event.Event, result event.Result) {
		starts++
		command := ev.Data().(upevents.StartTextCommand)
		switch starts {
		case 1:
			if command.AccessToken != "old-token" || command.Proxy != "http://old-proxy.invalid:8080" {
				t.Fatalf("first start=%+v", command)
			}
			result.Set(upevents.StartTextResult{StreamID: "stream-old"}, nil)
		case 2:
			if command.AccessToken != "new-token" || command.Proxy != "http://new-proxy.invalid:8080" {
				t.Fatalf("retry start=%+v", command)
			}
			result.Set(upevents.StartTextResult{StreamID: "stream-new"}, nil)
		default:
			t.Fatalf("unexpected start count=%d", starts)
		}
	})
	newPulls := 0
	upstream.Subscribe(upevents.TopicPullText, func(ev event.Event, result event.Result) {
		command := ev.Data().(upevents.PullTextCommand)
		switch command.StreamID {
		case "stream-old":
			result.Set(upevents.PullTextResult{Done: true, ErrorClass: upevents.ErrClassInvalidToken}, nil)
		case "stream-new":
			newPulls++
			if newPulls == 1 {
				result.Set(upevents.PullTextResult{Delta: "recovered"}, nil)
				return
			}
			result.Set(upevents.PullTextResult{Done: true, ConversationID: "up-1", AssistantMessageID: "message-1"}, nil)
		default:
			t.Fatalf("unexpected stream=%q", command.StreamID)
		}
	})
	upstream.Subscribe(upevents.TopicCancelText, func(_ event.Event, result event.Result) {
		result.Set(upevents.CancelTextResult{Cancelled: true}, nil)
	})

	tc, err := New(context.Background(), hub, background)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Teardown(context.Background())
	conversation, createErr := tc.store.CreateConversation("ops", "gpt-5", "", "", "account-1")
	if createErr != nil {
		t.Fatal(createErr)
	}
	value, err := tc.SendEvent(event.NewEvent(events.TopicStartTurn, "test", tempcommon.UnitID, nil, events.StartTurnCommand{
		OwnerID: "ops", ConversationID: conversation.ID, Content: "hello",
	})).Get()
	if err != nil {
		t.Fatal(err)
	}
	started, ok := value.(events.StartTurnResult)
	if !ok || started.TurnID == "" {
		t.Fatalf("start result=%+v", value)
	}

	var output string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value, err = tc.SendEvent(event.NewEvent(events.TopicPullTurn, "test", tempcommon.UnitID, nil, events.PullTurnCommand{
			OwnerID: "ops", ConversationID: conversation.ID, TurnID: started.TurnID, TimeoutMillis: 100,
		})).Get()
		if err != nil {
			t.Fatal(err)
		}
		update, ok := value.(events.PullTurnResult)
		if !ok {
			t.Fatalf("pull result=%+v", value)
		}
		output += update.Delta
		if update.Done {
			if update.ErrorClass != "" || output != "recovered" || starts != 2 {
				t.Fatalf("update=%+v output=%q starts=%d", update, output, starts)
			}
			select {
			case command := <-recorded:
				if !command.Success || command.ErrorClass != "" {
					t.Fatalf("record command=%+v", command)
				}
			case <-time.After(time.Second):
				t.Fatal("missing successful account result")
			}
			return
		}
	}
	t.Fatal("temporary turn did not complete")
}
