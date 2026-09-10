package aetherrelaycodex

import "testing"

func TestEmptyIncompleteRequiresExplicitZeroAndNoOutput(t *testing.T) {
	empty := []byte(`{"type":"response.incomplete","response":{"output":[],"usage":{"input_tokens":7,"output_tokens":0}}}`)
	if !EmptyIncomplete(empty, 0, false) {
		t.Fatal("explicit zero-output incomplete was not detected")
	}
	for _, payload := range [][]byte{
		[]byte(`{"type":"response.incomplete","response":{"output":[],"usage":{"output_tokens":2}}}`),
		[]byte(`{"type":"response.incomplete","response":{"output":[]}}`),
		[]byte(`{"type":"response.incomplete","response":{"output":[{"type":"message"}],"usage":{"output_tokens":0}}}`),
	} {
		if EmptyIncomplete(payload, 0, false) {
			t.Fatalf("valid incomplete classified empty: %s", payload)
		}
	}
	if EmptyIncomplete(empty, 1, false) || EmptyIncomplete(empty, 0, true) {
		t.Fatal("observed output was ignored")
	}
}

func TestMeaningfulOutputEventIncludesToolArgumentsDone(t *testing.T) {
	if !MeaningfulOutputEvent([]byte(`{"type":"response.function_call_arguments.done","arguments":"{\"city\":\"杭州\"}"}`)) {
		t.Fatal("full tool arguments done event was not treated as output")
	}
	if MeaningfulOutputEvent([]byte(`{"type":"response.function_call_arguments.done","arguments":""}`)) {
		t.Fatal("empty tool arguments done event was treated as output")
	}
}

func TestMeaningfulOutputEventIncludesDoneText(t *testing.T) {
	for _, eventType := range []string{"response.output_text.done", "response.reasoning.done", "response.reasoning_text.done", "response.reasoning_summary_text.done"} {
		payload := []byte(`{"type":"` + eventType + `","text":"generated"}`)
		if !MeaningfulOutputEvent(payload) {
			t.Fatalf("%s full text was not treated as output", eventType)
		}
	}
	if MeaningfulOutputEvent([]byte(`{"type":"response.output_text.done","text":"  "}`)) {
		t.Fatal("blank done text was treated as output")
	}
}

func TestMeaningfulOutputEventIncludesCompletedContentParts(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte(`{"type":"response.content_part.done","part":{"type":"output_text","text":"answer"}}`),
		[]byte(`{"type":"response.reasoning_summary_part.done","part":{"type":"summary_text","text":"analysis"}}`),
		[]byte(`{"type":"response.reasoning.done","summary":[{"type":"summary_text","text":"analysis"}]}`),
	} {
		if !MeaningfulOutputEvent(payload) {
			t.Fatalf("completed content was not treated as output: %s", payload)
		}
	}
	if MeaningfulOutputEvent([]byte(`{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`)) {
		t.Fatal("empty content part was treated as output")
	}
}

func TestEmptyIncompleteNativeResponse(t *testing.T) {
	if !EmptyIncompleteResponse([]byte(`{"object":"response","status":"incomplete","output":[],"usage":{"output_tokens":0}}`)) {
		t.Fatal("native empty incomplete response was not detected")
	}
	if EmptyIncompleteResponse([]byte(`{"object":"response","status":"incomplete","output":[],"usage":{"output_tokens":0.0}}`)) {
		t.Fatal("non-integer zero was accepted")
	}
}
