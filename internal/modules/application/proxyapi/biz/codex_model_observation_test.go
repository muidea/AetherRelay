package biz

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	"aetherrelay/internal/modules/application/proxyapi/pkg/usagetarget"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexSubstitutionLogsExecutingAccountAndKeepsSuccess(t *testing.T) {
	previousLimiter := codexModelWarnings
	codexModelWarnings = &codexmodel.WarningLimiter{}
	defer func() { codexModelWarnings = previousLimiter }()
	for _, mode := range []string{"complete", "stream", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			hub := event.NewHub(24)
			background := task.NewBackgroundRoutine(8)
			t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
			accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
			accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, r event.Result) {
				r.Set(accevents.AcquireResult{AccountID: "slot-" + mode, AccountLabel: "operator@example.com", AccessToken: "private-token", LeaseID: "lease"}, nil)
			})
			accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, r event.Result) { r.Set(accevents.ReleaseResult{Released: true}, nil) })
			feedback := make(chan accevents.RecordResultCommand, 4)
			accounts.Subscribe(accevents.TopicRecordResult, func(e event.Event, r event.Result) {
				feedback <- e.Data().(accevents.RecordResultCommand)
				r.Set(accevents.RecordResultResult{}, nil)
			})
			accounts.Subscribe(accevents.TopicRecordTransportCapability, func(_ event.Event, r event.Result) { r.Set(accevents.RecordTransportCapabilityResult{}, nil) })
			up := event.NewSimpleObserver(upcommon.UnitID, hub)
			observation := codexmodel.New("gpt-6.1-sol", "gpt-6-luna")
			attempt := upevents.HTTPAttempt{Response: upevents.HTTPResponseObservation{Observation: observation}}
			body := []byte(`{"object":"response","model":"gpt-6-luna","output":[],"usage":{"input_tokens":1}}`)
			terminal := []byte(`{"type":"response.completed","response":{"model":"gpt-6-luna","output":[],"usage":{"input_tokens":1}}}`)
			up.Subscribe(upevents.TopicComplete, func(_ event.Event, r event.Result) { r.Set(upevents.CompleteResult{Body: body, Attempt: attempt}, nil) })
			up.Subscribe(upevents.TopicStart, func(_ event.Event, r event.Result) {
				r.Set(upevents.StartResult{StreamID: "stream", Attempt: attempt}, nil)
			})
			up.Subscribe(upevents.TopicPull, func(_ event.Event, r event.Result) {
				r.Set(upevents.PullResult{Data: append([]byte("data: "), terminal...), Progress: attempt.Response, Done: true}, nil)
			})
			up.Subscribe(upevents.TopicCancel, func(_ event.Event, r event.Result) {
				r.Set(upevents.CancelResult{Progress: attempt.Response, Cancelled: true}, nil)
			})
			up.Subscribe(upevents.TopicWSOpen, func(_ event.Event, r event.Result) { r.Set(upevents.WSOpenResult{SessionID: "ws"}, nil) })
			up.Subscribe(upevents.TopicWSSend, func(_ event.Event, r event.Result) { r.Set(upevents.WSSendResult{Sent: true}, nil) })
			up.Subscribe(upevents.TopicWSPull, func(_ event.Event, r event.Result) { r.Set(upevents.WSPullResult{Payload: terminal}, nil) })
			proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
			ctx := usagetarget.WithTracker(context.Background())
			req := codexresponses.Request{Model: "gpt-6.1-sol", Body: []byte(`{"model":"gpt-6.1-sol"}`)}
			for i := 0; i < 2; i++ {
				if mode == "websocket" && i > 0 {
					// The executing slot must survive a fresh turn's diagnostic context.
					ctx = usagetarget.WithTracker(context.Background())
				}
				var err error
				switch mode {
				case "complete":
					_, err = proxy.CompleteCodexResponses(ctx, req)
				case "stream":
					err = proxy.StreamCodexResponses(ctx, req, nil, func([]byte) error { return nil })
				case "websocket":
					if i == 0 {
						_, err = proxy.OpenCodexWebsocket(ctx, codexresponses.WebsocketOpenRequest{Model: req.Model})
					}
					if err == nil {
						err = proxy.SendCodexWebsocket(ctx, "ws", req.Body)
					}
					if err == nil {
						_, err = proxy.PullCodexWebsocket(ctx, "ws")
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := <-feedback; !got.Success || got.AccountID != "slot-"+mode {
					t.Fatal("substitution changed health feedback", got)
				}
				if got := usagetarget.Snapshot(ctx); !got.ModelSubstituted || got.AccountLabel != "operator@example.com" {
					t.Fatal(got)
				}
			}
			text := logs.String()
			if strings.Count(text, "Codex upstream model substituted") != 1 || !strings.Contains(text, "account_label=operator@example.com") || !strings.Contains(text, "account_id=slot-"+mode) || strings.Contains(text, "private-token") {
				t.Fatal(text)
			}
		})
	}
}
