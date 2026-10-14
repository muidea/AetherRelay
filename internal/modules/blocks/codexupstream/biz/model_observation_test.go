package biz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aetherrelay/internal/modules/blocks/codexupstream/pkg/events"
	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
	"github.com/muidea/magicCommon/event"
)

func TestModelObservationAcrossBufferedAndStreamingReadPaths(t *testing.T) {
	for _, tc := range []struct{ name, final, want string }{
		{"terminal-match", `,"model":"gpt-6.1-sol"`, "gpt-6.1-sol"},
		{"terminal-mismatch", `,"model":"gpt-6-luna"`, "gpt-6-luna"},
		{"missing-terminal-model", "", "gpt-6-luna"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := "data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-6-luna\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"output\":[],\"usage\":{\"input_tokens\":1}" + tc.final + "}}\n\n"
			observation := events.HTTPResponseObservation{Observation: codexmodel.New("gpt-6.1-sol", "")}
			resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Safety-Buffering-Faster-Model": {"gpt-6-luna"}}, Body: io.NopCloser(strings.NewReader(payload))}
			_, class, _, _, err := completedResponse(resp, 1<<20, &observation)
			if err != nil || class != "" || observation.ResponseModel != tc.want {
				t.Fatal(observation, class, err)
			}
			stream := &responseStream{updates: make(chan streamUpdate, 16), progress: events.HTTPResponseObservation{Observation: codexmodel.New("gpt-6.1-sol", "")}}
			(&Upstream{}).runStream(context.Background(), "test", stream, io.NopCloser(strings.NewReader(payload)), 1<<20)
			if got := stream.observation(); got.ResponseModel != tc.want || got.ModelSubstituted != observation.ModelSubstituted {
				t.Fatal(got, observation)
			}
		})
	}
}

func TestFinalWireModelWithoutFullArchiveIgnoresFasterModelHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || codexmodel.RequestModel(body) != "gpt-6.1-sol" {
			t.Error("unexpected wire model", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Safety-Buffering-Faster-Model", "gpt-6-luna")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":1}}}\n\n")
	}))
	defer server.Close()
	previous := responsesURL
	responsesURL = server.URL
	defer func() { responsesURL = previous }()
	ctx := context.Background()
	r := event.NewResult(events.TopicComplete, "test", "upstream")
	(&Upstream{}).handleComplete(event.NewEventWithContext(events.TopicComplete, "test", "upstream", nil, ctx, events.CompleteCommand{AccessToken: "fixture", Body: []byte(`{"model":"gpt-6.1-sol","input":[]}`)}), r)
	value, err := r.Get()
	if err != nil {
		t.Fatal(err)
	}
	got := value.(events.CompleteResult)
	if got.ErrorClass != "" || got.Attempt.Response.UpstreamRequestModel != "gpt-6.1-sol" || got.Attempt.Response.ResponseModel != "gpt-6.1-sol" || got.Attempt.Response.ModelSubstituted || len(got.Attempt.Request.Body) != 0 {
		t.Fatal(got)
	}
}
