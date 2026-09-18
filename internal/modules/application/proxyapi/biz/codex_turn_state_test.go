package biz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	aetherrelayconfig "aetherrelay/internal/pkg/aetherrelayconfig"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// codexTurnStateProxy builds the runtime with a real loaded configuration so the
// CP-HDR-023 switches go through the same parser, defaults and validation as a
// deployment. An empty section keeps the zero-value configuration that exercises
// the built-in fallback.
func codexTurnStateProxy(t *testing.T, codexOAuth string) *Proxy {
	t.Helper()
	proxy := &Proxy{}
	if codexOAuth == "" {
		return proxy
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "server:\n  listen_addr: 127.0.0.1:18080\ncodex_oauth:\n" + codexOAuth
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := aetherrelayconfig.Load(path)
	if err != nil {
		t.Fatalf("load codex_oauth test config: %v", err)
	}
	proxy.config = cfg
	return proxy
}

func codexTurnStateHeader(value string) []upevents.Header {
	return []upevents.Header{{Name: "X-Codex-Turn-State", Value: value}}
}

func mustCodexTurnStateScope(t *testing.T, accountID string, fingerprint upevents.CodexFingerprint, sessionScope string) codexTurnStateScope {
	t.Helper()
	scope, ok := codexTurnStateScopeFor(accountID, fingerprint, sessionScope)
	if !ok {
		t.Fatalf("CP-HDR-022 scope rejected for account=%q session=%q", accountID, sessionScope)
	}
	return scope
}

// CP-HDR-022: off/device keep the upstream Session-Id granularity, while
// session/full stay per downstream session because the upstream collapses those
// sessions onto one account level Session-Id.
func TestCodexTurnStateScopeGranularityMatrix(t *testing.T) {
	cases := []struct {
		mode        string
		sessionID   string
		wantMode    string
		wantSession string
	}{
		{mode: "", wantMode: ""},
		{mode: "device", wantMode: "device"},
		{mode: "session", sessionID: "account-session", wantMode: "session", wantSession: "account-session"},
		{mode: "full", sessionID: "account-session", wantMode: "full", wantSession: "account-session"},
		{mode: "SESSION", sessionID: "account-session", wantMode: "session", wantSession: "account-session"},
	}
	for _, testCase := range cases {
		fingerprint := upevents.CodexFingerprint{Mode: testCase.mode, SessionID: testCase.sessionID}
		scope, ok := codexTurnStateScopeFor("account-a", fingerprint, "session-hash")
		if !ok {
			t.Fatalf("CP-HDR-022 mode=%q scope rejected", testCase.mode)
		}
		if scope.accountID != "account-a" || scope.sessionScope != "session-hash" || scope.mode != testCase.wantMode || scope.sessionID != testCase.wantSession {
			t.Fatalf("CP-HDR-022 mode=%q scope=%+v", testCase.mode, scope)
		}
	}
	// Keeping downstream sessions apart is what stops session/full convergence
	// from merging two clients into one record.
	converged := upevents.CodexFingerprint{Mode: "session", SessionID: "account-session"}
	first, _ := codexTurnStateScopeFor("account-a", converged, "session-a")
	second, _ := codexTurnStateScopeFor("account-a", converged, "session-b")
	if first == second {
		t.Fatalf("CP-HDR-022 converged sessions share a record unit: %+v", first)
	}
	// The scope is also the CP-HDR-020 isolation boundary.
	if other, _ := codexTurnStateScopeFor("account-b", upevents.CodexFingerprint{}, "session-a"); other == first {
		t.Fatalf("CP-HDR-020 accounts share a record unit: %+v", first)
	}
	// Without a downstream session or a minting account there is no record unit.
	if _, ok := codexTurnStateScopeFor("account-a", upevents.CodexFingerprint{}, ""); ok {
		t.Fatal("CP-HDR-022 accepted a session-less record unit")
	}
	if _, ok := codexTurnStateScopeFor("", upevents.CodexFingerprint{}, "session-a"); ok {
		t.Fatal("CP-HDR-022 accepted a record unit without a minting account")
	}
}

// CP-HDR-022: a session replays its own latest observation once the client stops
// sending the header.
func TestCodexTurnStateSessionReplayFillsMissingHeader(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  default_turn_state: state-default\n")
	proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, "session-a", codexTurnStateHeader("state-1"))

	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "state-1" || source != codexresponses.TurnStateSourceSession {
		t.Fatalf("CP-HDR-022 replay state=%q source=%q", state, source)
	}
	// The newest observation wins over the previous one.
	proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, "session-a", codexTurnStateHeader("state-2"))
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "state-2" || source != codexresponses.TurnStateSourceSession {
		t.Fatalf("CP-HDR-022 latest state=%q source=%q", state, source)
	}
	for _, isolation := range []struct {
		name         string
		accountID    string
		sessionScope string
	}{
		{name: "other session", accountID: "account-a", sessionScope: "session-b"},
		{name: "other account", accountID: "account-b", sessionScope: "session-a"},
	} {
		state, source := proxy.resolveCodexSessionTurnState(isolation.accountID, upevents.CodexFingerprint{}, isolation.sessionScope, "")
		if state != "state-default" || source != codexresponses.TurnStateSourceDefault {
			t.Fatalf("CP-HDR-020 %s state=%q source=%q", isolation.name, state, source)
		}
	}
}

// CP-HDR-022: only an omitted header is filled. A value that CP-HDR-020 stripped
// stays empty; it is never replaced by the record or by the default.
func TestCodexTurnStateStrippedValueIsNeverReplaced(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  default_turn_state: state-default\n")
	proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, "session-a", codexTurnStateHeader("state-a"))
	device := upevents.CodexFingerprint{Mode: "device", InstallationID: "installation"}
	proxy.noteCodexSessionTurnState(mustCodexTurnStateScope(t, "account-b", device, "session-a"), "state-b")

	state, source := proxy.resolveCodexSessionTurnState("account-b", device, "session-a", "state-a")
	if state != "" || source != codexresponses.TurnStateSourceStripped {
		t.Fatalf("CP-HDR-020 stripped value was replaced: state=%q source=%q", state, source)
	}
	// A request without a declared conversation has no record unit, but an
	// omitted header still falls back instead of failing the request.
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "", ""); state != "state-default" || source != codexresponses.TurnStateSourceDefault {
		t.Fatalf("CP-HDR-022 session-less state=%q source=%q", state, source)
	}
	// A request without a declared conversation never records anything, so two
	// unrelated stateless requests cannot inherit each other's state.
	recorded := len(proxy.codexSessionTurnStates)
	proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, "", codexTurnStateHeader("state-stateless"))
	if len(proxy.codexSessionTurnStates) != recorded {
		t.Fatalf("CP-HDR-022 stateless request was recorded: %+v", proxy.codexSessionTurnStates)
	}
}

// CP-HDR-022: a client supplied value wins and becomes the session record, and
// it stays inside the minting account scope (CP-HDR-020).
func TestCodexTurnStateClientValueWinsAndIsRemembered(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  default_turn_state: state-default\n")
	state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", "state-client")
	if state != "state-client" || source != codexresponses.TurnStateSourceClient {
		t.Fatalf("CP-HDR-022 client state=%q source=%q", state, source)
	}
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "state-client" || source != codexresponses.TurnStateSourceSession {
		t.Fatalf("CP-HDR-022 remembered state=%q source=%q", state, source)
	}
	if state, source := proxy.resolveCodexSessionTurnState("account-b", upevents.CodexFingerprint{}, "session-a", ""); state != "state-default" || source != codexresponses.TurnStateSourceDefault {
		t.Fatalf("CP-HDR-020 record crossed accounts: state=%q source=%q", state, source)
	}
}

// CP-HDR-022: the fill is the configured value, or the built-in one when it is
// set; an unset default means the attempt sends nothing, and a default is never
// written back into the session table.
func TestCodexTurnStateDefaultIsConfiguredAndNeverRecorded(t *testing.T) {
	unset := codexTurnStateProxy(t, "")
	if aetherrelayconfig.DefaultCodexTurnState != "" {
		t.Fatalf("CP-HDR-023 unexpected built-in default in this deployment: %q", aetherrelayconfig.DefaultCodexTurnState)
	}
	if state, source := unset.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "" || source != codexresponses.TurnStateSourceAbsent {
		t.Fatalf("CP-HDR-023 unset default state=%q source=%q", state, source)
	}
	if len(unset.codexSessionTurnStates) != 0 || unset.codexSessionTurnStateBytes != 0 {
		t.Fatalf("CP-HDR-023 unset default was recorded: %+v", unset.codexSessionTurnStates)
	}

	configured := codexTurnStateProxy(t, "  default_turn_state: state-configured\n")
	if state, source := configured.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "state-configured" || source != codexresponses.TurnStateSourceDefault {
		t.Fatalf("CP-HDR-022 configured state=%q source=%q", state, source)
	}
	// An upstream echo of the configured default is not an observation either.
	configured.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, "session-a", codexTurnStateHeader("state-configured"))
	if len(configured.codexSessionTurnStates) != 0 {
		t.Fatalf("CP-HDR-023 configured default was recorded: %+v", configured.codexSessionTurnStates)
	}
}

// CP-HDR-023: disabling the fallback stops the fill but keeps recording.
func TestCodexTurnStateFallbackDisabledStillRecords(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  turn_state_fallback: false\n")
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "" || source != codexresponses.TurnStateSourceAbsent {
		t.Fatalf("CP-HDR-023 disabled state=%q source=%q", state, source)
	}
	proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, "session-a", codexTurnStateHeader("state-1"))
	scope := mustCodexTurnStateScope(t, "account-a", upevents.CodexFingerprint{}, "session-a")
	if state := proxy.sessionCodexTurnState(scope); state != "state-1" {
		t.Fatalf("CP-HDR-023 disabled record=%q", state)
	}
	// Re-enabling through a hot reload replays the record kept while disabled.
	proxy.config.CodexOAuth = codexTurnStateProxy(t, "  turn_state_fallback: true\n").config.CodexOAuth
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "state-1" || source != codexresponses.TurnStateSourceSession {
		t.Fatalf("CP-HDR-023 re-enabled state=%q source=%q", state, source)
	}
}

// CP-HDR-023: both budgets hold and eviction drops the oldest observation.
func TestCodexTurnStateSessionRecordsRemainBounded(t *testing.T) {
	proxy := codexTurnStateProxy(t, "")
	for index := 0; index <= codexSessionTurnStateMaxEntries; index++ {
		proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, fmt.Sprintf("session-%d", index), codexTurnStateHeader(fmt.Sprintf("state-%d", index)))
	}
	if len(proxy.codexSessionTurnStates) > codexSessionTurnStateMaxEntries {
		t.Fatalf("CP-HDR-023 session entries=%d", len(proxy.codexSessionTurnStates))
	}
	newest := fmt.Sprintf("state-%d", codexSessionTurnStateMaxEntries)
	if state := proxy.sessionCodexTurnState(mustCodexTurnStateScope(t, "account-a", upevents.CodexFingerprint{}, fmt.Sprintf("session-%d", codexSessionTurnStateMaxEntries))); state != newest {
		t.Fatalf("CP-HDR-023 newest record was evicted: %q", state)
	}
	if state := proxy.sessionCodexTurnState(mustCodexTurnStateScope(t, "account-a", upevents.CodexFingerprint{}, "session-0")); state != "" {
		t.Fatalf("CP-HDR-023 oldest record survived: %q", state)
	}

	// The byte budget evicts independently of the entry count.
	value := strings.Repeat("x", aetherrelayconfig.MaxCodexTurnStateBytes)
	byteProxy := codexTurnStateProxy(t, "")
	for index := 0; index <= codexSessionTurnStateMaxBytes/len(value); index++ {
		byteProxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, fmt.Sprintf("session-%d", index), codexTurnStateHeader(value))
	}
	if byteProxy.codexSessionTurnStateBytes > codexSessionTurnStateMaxBytes {
		t.Fatalf("CP-HDR-023 session bytes=%d", byteProxy.codexSessionTurnStateBytes)
	}
	if byteProxy.codexSessionTurnStateBytes != len(byteProxy.codexSessionTurnStates)*len(value) {
		t.Fatalf("CP-HDR-023 byte accounting drifted: %d bytes for %d entries", byteProxy.codexSessionTurnStateBytes, len(byteProxy.codexSessionTurnStates))
	}
	// Oversized values are rejected instead of consuming the budget.
	oversized := value + "x"
	oversizedScope := mustCodexTurnStateScope(t, "account-a", upevents.CodexFingerprint{}, "session-oversized")
	byteProxy.noteCodexSessionTurnState(oversizedScope, oversized)
	if _, found := byteProxy.codexSessionTurnStates[oversizedScope]; found {
		t.Fatal("CP-HDR-023 oversized value was recorded")
	}
}

func TestCodexTurnStateSessionRecordConcurrency(t *testing.T) {
	proxy := codexTurnStateProxy(t, "")
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < 64; index++ {
				session := fmt.Sprintf("session-%d", index%16)
				proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, session, codexTurnStateHeader(fmt.Sprintf("state-%d-%d", worker, index)))
				proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, session, "")
			}
		}(worker)
	}
	wait.Wait()
	total := 0
	for _, entry := range proxy.codexSessionTurnStates {
		total += len(entry.state)
	}
	if total != proxy.codexSessionTurnStateBytes {
		t.Fatalf("CP-HDR-023 byte accounting drifted: %d bytes for %d observed", proxy.codexSessionTurnStateBytes, total)
	}
}

// CP-HDR-022: the filled value must reach the upstream command, and the result
// must report where it came from.
func TestCompleteCodexResponsesSendsFilledTurnState(t *testing.T) {
	for name, testCase := range map[string]struct {
		observation  string
		sessionScope string
		wantState    string
		wantSource   codexresponses.TurnStateSource
	}{
		"session record": {observation: "state-observed", sessionScope: "scope-a", wantState: "state-observed", wantSource: codexresponses.TurnStateSourceSession},
		"no observation": {
			sessionScope: "scope-a",
			wantState:    "",
			wantSource:   codexresponses.TurnStateSourceAbsent,
		},
		"no declared conversation": {
			sessionScope: "",
			wantState:    "",
			wantSource:   codexresponses.TurnStateSourceAbsent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			hub := event.NewHub(24)
			background := task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
				result.Set(accevents.AcquireResult{AccountID: "account-a", AccessToken: "token-a", LeaseID: "lease-a", FingerprintMode: accevents.FingerprintModeOff}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{Released: true}, nil) })
			accounts.Subscribe(accevents.TopicRecordResult, func(_ event.Event, result event.Result) { result.Set(accevents.RecordResultResult{}, nil) })
			commands := make(chan upevents.CompleteCommand, 1)
			upstream := event.NewSimpleObserver(upcommon.UnitID, hub)
			upstream.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
				commands <- ev.Data().(upevents.CompleteCommand)
				result.Set(upevents.CompleteResult{Body: []byte(`{"id":"resp-a"}`)}, nil)
			})
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background), codexTurnStates: map[string]codexTurnStateOrigin{}}
			if testCase.observation != "" {
				proxy.noteCodexTurnState("account-a", upevents.CodexFingerprint{}, testCase.sessionScope, codexTurnStateHeader(testCase.observation))
			}
			completed, err := proxy.CompleteCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-test", Body: []byte(`{"model":"gpt-test"}`), SessionHash: "client-session", SessionScope: testCase.sessionScope})
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
			command := <-commands
			if command.TurnState != testCase.wantState {
				t.Fatalf("CP-HDR-022 upstream turn state=%q want %q", command.TurnState, testCase.wantState)
			}
			if completed.TurnStateSource != testCase.wantSource {
				t.Fatalf("CP-HDR-022 result source=%q want %q", completed.TurnStateSource, testCase.wantSource)
			}
		})
	}
}

// CP-HDR-022: a request that declares no conversation has no record unit, so an
// unrelated stateless request can never inherit its observed state.
func TestCodexTurnStateStatelessRequestsNeverShareARecord(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  default_turn_state: state-default\n")
	scope, ok := codexTurnStateScopeFor("account-a", upevents.CodexFingerprint{}, "")
	if ok {
		t.Fatalf("CP-HDR-022 stateless request got a record unit: %+v", scope)
	}

	// Even when the client supplies a value, a stateless request leaves no
	// record behind for the next one.
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "", "state-from-client"); state != "state-from-client" || source != codexresponses.TurnStateSourceClient {
		t.Fatalf("CP-HDR-022 stateless client state=%q source=%q", state, source)
	}
	if len(proxy.codexSessionTurnStates) != 0 {
		t.Fatalf("CP-HDR-022 stateless request was recorded: %+v", proxy.codexSessionTurnStates)
	}
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "", ""); state != "state-default" || source != codexresponses.TurnStateSourceDefault {
		t.Fatalf("CP-HDR-022 stateless replay state=%q source=%q", state, source)
	}
}

// CP-HDR-022 强制开关：启用后只有有效默认值出站，客户端值与会话记录都不参与；
// 强制值不写记录、上游观测照旧记录，因此关闭开关即可恢复原链路。
func TestCodexTurnStateForceDefaultWinsOverClientAndRecord(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  turn_state_force_default: true\n  default_turn_state: state-forced\n")
	device := upevents.CodexFingerprint{Mode: "device", InstallationID: "installation"}
	scope := mustCodexTurnStateScope(t, "account-a", device, "session-a")
	proxy.noteCodexSessionTurnState(scope, "state-record")

	state, source := proxy.resolveCodexSessionTurnState("account-a", device, "session-a", "state-from-client")
	if state != "state-forced" || source != codexresponses.TurnStateSourceForced {
		t.Fatalf("CP-HDR-022 forced state=%q source=%q", state, source)
	}
	if !codexresponses.ValidTurnStateSource(source) || !codexresponses.TurnStateFallback(source) {
		t.Fatalf("CP-HDR-022 forced source must be valid and proxy supplied: %q", source)
	}
	// 强制值与被忽略的客户端值都不能污染会话记录。
	if recorded := proxy.sessionCodexTurnState(scope); recorded != "state-record" {
		t.Fatalf("CP-HDR-022 forced value polluted the session record: %q", recorded)
	}

	// 上游观测仍然记录：关闭开关后立即恢复按记录回填。
	proxy.noteCodexTurnState("account-a", device, "session-a", codexTurnStateHeader("state-observed"))
	proxy.config.CodexOAuth.TurnStateForceDefault = false
	if state, source := proxy.resolveCodexSessionTurnState("account-a", device, "session-a", ""); state != "state-observed" || source != codexresponses.TurnStateSourceSession {
		t.Fatalf("CP-HDR-022 after disabling force state=%q source=%q", state, source)
	}
}

// CP-HDR-022：force 优先于 turn_state_fallback: false，两个开关同时存在时以 force 为准。
func TestCodexTurnStateForceDefaultBeatsDisabledFallback(t *testing.T) {
	proxy := codexTurnStateProxy(t, "  turn_state_fallback: false\n  turn_state_force_default: true\n  default_turn_state: state-forced\n")
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "state-forced" || source != codexresponses.TurnStateSourceForced {
		t.Fatalf("CP-HDR-022 force with fallback disabled state=%q source=%q", state, source)
	}
	// 开关关闭、fallback 也关闭时保持既有语义：不回填、不替换客户端值。
	proxy.config.CodexOAuth.TurnStateForceDefault = false
	if state, source := proxy.resolveCodexSessionTurnState("account-a", upevents.CodexFingerprint{}, "session-a", ""); state != "" || source != codexresponses.TurnStateSourceAbsent {
		t.Fatalf("CP-HDR-022 fallback disabled state=%q source=%q", state, source)
	}
}
