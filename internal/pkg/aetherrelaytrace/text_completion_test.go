package aetherrelaytrace

import (
	completion "aetherrelay/internal/pkg/aetherrelaycompletion"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestTextCompletionTraceKeepsEvidenceWithoutContent(t *testing.T) {
	const privateText = "private answer body"
	ctx := WithCollector(context.Background())
	collector := FromContext(ctx)
	collector.Begin("chatgptweb", "account", "label", "model")
	evidence := completion.Text{Reason: completion.Protocol, SSEBytes: 512, TextBytes: len(privateText), OutputBytes: len(privateText), TextSHA256: completion.Hash(privateText), SSESHA256: completion.Hash("wire"), Events: 3, ResponseStarted: true}
	collector.ObserveTextCompletion(evidence, "text_protocol")
	trace := collector.Snapshot()
	if len(trace.Attempts) != 1 || trace.Attempts[0].TextCompletion != evidence || !trace.Attempts[0].Failed || trace.ObservedFailures != 1 {
		t.Fatal(trace)
	}
	collector.ObserveTextCompletion(evidence, "text_protocol")
	if collector.Snapshot().ObservedFailures != 1 {
		t.Fatal("failure counted twice")
	}
	raw, err := json.Marshal(trace)
	if err != nil || strings.Contains(string(raw), privateText) || !strings.Contains(string(raw), completion.Hash(privateText)) {
		t.Fatal(string(raw), err)
	}
}
