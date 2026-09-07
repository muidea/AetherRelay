package biz

import (
	"context"
	"fmt"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	acccommon "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	upcommon "aetherrelay/internal/modules/blocks/codexupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

func TestCodexModelNotFoundBoundedSameModelFailover(t *testing.T) {
	// CP-FAIL-018, CP-COMPACT-005: model-unavailable is a narrow exception.
	for _, transport := range []string{"complete", "stream", "compact"} {
		for _, tc := range []struct {
			name, previous, turnState string
			class                     upevents.ErrorClass
			alwaysFail                bool
			want                      int
		}{
			{name: "recover", class: upevents.ErrorModelNotFound, want: 2},
			{name: "exhaust", class: upevents.ErrorModelNotFound, alwaysFail: true, want: 3},
			{name: "previous", previous: `,"previous_response_id":"resp_old"`, class: upevents.ErrorModelNotFound, want: 1},
			{name: "turn state", turnState: "opaque", class: upevents.ErrorModelNotFound, want: 1},
			{name: "ordinary404", class: upevents.ErrorInvalidRequest, want: 1},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				hub := event.NewHub(24)
				background := task.NewBackgroundRoutine(8)
				t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
				accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
				attempts, releases, records := 0, 0, 0
				accounts.Subscribe(accevents.TopicAcquire, func(ev event.Event, result event.Result) {
					cmd := ev.Data().(accevents.AcquireCommand)
					if cmd.Model != "gpt-5.5" || len(cmd.Exclude) != attempts {
						t.Errorf("acquire = %+v", cmd)
					}
					attempts++
					result.Set(accevents.AcquireResult{AccountID: fmt.Sprint(attempts), AccessToken: "test", LeaseID: fmt.Sprint(attempts)}, nil)
				})
				accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { releases++; result.Set(accevents.ReleaseResult{}, nil) })
				accounts.Subscribe(accevents.TopicRecordTransportCapability, func(_ event.Event, result event.Result) { result.Set(accevents.RecordTransportCapabilityResult{}, nil) })
				accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
					cmd := ev.Data().(accevents.RecordResultCommand)
					records++
					if cmd.Model != "gpt-5.5" || (!cmd.Success && tc.class == upevents.ErrorModelNotFound && (cmd.AvailabilityNeutral || cmd.ErrorClass != accevents.ErrorModelNotFound)) {
						t.Errorf("record=%+v", cmd)
					}
					result.Set(accevents.RecordResultResult{}, nil)
				})
				up := event.NewSimpleObserver(upcommon.UnitID, hub)
				body := []byte(`{"model":"gpt-5.5"` + tc.previous + `}`)
				fail := func() bool { return attempts == 1 || tc.alwaysFail }
				safe := upevents.SafeError{Code: "model_not_found", Type: "invalid_request_error", Message: "model unavailable"}
				up.Subscribe(upevents.TopicComplete, func(ev event.Event, result event.Result) {
					if string(ev.Data().(upevents.CompleteCommand).Body) != string(body) {
						t.Error("body/model changed")
					}
					if fail() {
						result.Set(upevents.CompleteResult{ErrorClass: tc.class, HTTPStatus: 404, SafeError: safe}, nil)
					} else {
						result.Set(upevents.CompleteResult{Body: []byte(`{"output":[{}]}`)}, nil)
					}
				})
				up.Subscribe(upevents.TopicCompact, func(ev event.Event, result event.Result) {
					if string(ev.Data().(upevents.CompactCommand).Body) != string(body) {
						t.Error("compact body/model changed")
					}
					if fail() {
						result.Set(upevents.CompactResult{ErrorClass: tc.class, HTTPStatus: 404, SafeError: safe}, nil)
					} else {
						result.Set(upevents.CompactResult{Body: []byte(`{"output":[{"type":"compaction"}]}`)}, nil)
					}
				})
				up.Subscribe(upevents.TopicStart, func(ev event.Event, result event.Result) {
					if string(ev.Data().(upevents.StartCommand).Body) != string(body) {
						t.Error("stream body/model changed")
					}
					if fail() {
						result.Set(upevents.StartResult{ErrorClass: tc.class, HTTPStatus: 404, SafeError: safe}, nil)
					} else {
						result.Set(upevents.StartResult{StreamID: "stream"}, nil)
					}
				})
				up.Subscribe(upevents.TopicPull, func(_ event.Event, result event.Result) { result.Set(upevents.PullResult{Done: true}, nil) })
				up.Subscribe(upevents.TopicCancel, func(_ event.Event, result event.Result) {
					if result != nil {
						result.Set(upevents.CancelResult{}, nil)
					}
				})
				proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
				request := codexresponses.Request{Model: "gpt-5.5", Body: body, TurnState: tc.turnState}
				var err error
				switch transport {
				case "complete":
					_, err = proxy.CompleteCodexResponses(context.Background(), request)
				case "compact":
					_, err = proxy.CompleteCodexCompact(context.Background(), request)
				case "stream":
					err = proxy.StreamCodexResponses(context.Background(), request, nil, nil)
				}
				if attempts != tc.want || releases != attempts {
					t.Fatalf("attempts=%d releases=%d want=%d", attempts, releases, tc.want)
				}
				if tc.want == 2 {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					failure, ok := codexresponses.AsFailure(err)
					if !ok || failure.HTTPStatus != 404 || failure.UpstreamCode != "model_not_found" {
						t.Fatalf("lost upstream error: %+v", err)
					}
				}
				if tc.class == upevents.ErrorModelNotFound && records != attempts {
					t.Errorf("missing records: %d", records)
				}
			})
		}
	}
}

func TestCodexModelNotFoundStreamAfterOutputDoesNotRetry(t *testing.T) {
	// CP-STREAM-003 / CP-FAIL-018: neither SSE failure nor client cancellation
	// may trigger a replay once business output has been delivered.
	hub := event.NewHub(24)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	attempts, releases, records, pulls := 0, 0, 0, 0
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		attempts++
		result.Set(accevents.AcquireResult{AccountID: "account", AccessToken: "test", LeaseID: "lease"}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { releases++; result.Set(accevents.ReleaseResult{}, nil) })
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		records++
		if ev.Data().(accevents.RecordResultCommand).ErrorClass != accevents.ErrorModelNotFound {
			t.Error("missing typed record")
		}
		result.Set(accevents.RecordResultResult{}, nil)
	})
	up := event.NewSimpleObserver(upcommon.UnitID, hub)
	up.Subscribe(upevents.TopicStart, func(_ event.Event, result event.Result) { result.Set(upevents.StartResult{StreamID: "stream"}, nil) })
	up.Subscribe(upevents.TopicPull, func(_ event.Event, result event.Result) {
		pulls++
		if pulls == 1 {
			result.Set(upevents.PullResult{Data: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")}, nil)
		} else {
			result.Set(upevents.PullResult{Done: true, ErrorClass: upevents.ErrorModelNotFound, SafeError: upevents.SafeError{Code: "model_not_found"}}, nil)
		}
	})
	up.Subscribe(upevents.TopicCancel, func(_ event.Event, result event.Result) {
		if result != nil {
			result.Set(upevents.CancelResult{}, nil)
		}
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	emitted := 0
	err := proxy.StreamCodexResponses(context.Background(), codexresponses.Request{Model: "gpt-5.5", Body: []byte(`{"model":"gpt-5.5"}`)}, nil, func([]byte) error { emitted++; return nil })
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure.Kind != codexresponses.KindModelNotFound || failure.HTTPStatus != 404 || attempts != 1 || releases != 1 || records != 1 || emitted != 1 {
		t.Fatalf("err=%v attempts=%d releases=%d records=%d emitted=%d", err, attempts, releases, records, emitted)
	}
}

func TestCodexModelNotFoundWebsocketDoesNotDisableTransportOrMigrate(t *testing.T) {
	// CP-FAIL-018: model failure is not evidence of unsupported WS transport.
	hub := event.NewHub(24)
	background := task.NewBackgroundRoutine(8)
	t.Cleanup(func() { background.Shutdown(nil); hub.Terminate(context.Background()) })
	accounts := event.NewSimpleObserver(acccommon.UnitID, hub)
	attempts, recorded, disabled := 0, 0, 0
	accounts.Subscribe(accevents.TopicAcquire, func(_ event.Event, result event.Result) {
		attempts++
		result.Set(accevents.AcquireResult{AccountID: "account", AccessToken: "test", LeaseID: "lease"}, nil)
	})
	accounts.Subscribe(accevents.TopicRelease, func(_ event.Event, result event.Result) { result.Set(accevents.ReleaseResult{}, nil) })
	accounts.Subscribe(accevents.TopicRecordResult, func(ev event.Event, result event.Result) {
		recorded++
		if ev.Data().(accevents.RecordResultCommand).ErrorClass != accevents.ErrorModelNotFound {
			t.Error("wrong class")
		}
		result.Set(accevents.RecordResultResult{}, nil)
	})
	accounts.Subscribe(accevents.TopicRecordTransportCapability, func(_ event.Event, result event.Result) {
		disabled++
		result.Set(accevents.RecordTransportCapabilityResult{}, nil)
	})
	up := event.NewSimpleObserver(upcommon.UnitID, hub)
	up.Subscribe(upevents.TopicWSOpen, func(_ event.Event, result event.Result) {
		result.Set(upevents.WSOpenResult{ErrorClass: upevents.ErrorModelNotFound, HTTPStatus: 404}, nil)
	})
	proxy := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, background)}
	_, err := proxy.OpenCodexWebsocket(context.Background(), codexresponses.WebsocketOpenRequest{Model: "gpt-5.5"})
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure.Kind != codexresponses.KindModelNotFound || attempts != 1 || recorded != 1 || disabled != 0 {
		t.Fatalf("err=%v attempts=%d recorded=%d transport changes=%d", err, attempts, recorded, disabled)
	}
}
