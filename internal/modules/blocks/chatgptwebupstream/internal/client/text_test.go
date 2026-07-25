package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

type textDoer struct {
	conversationRequest *http.Request
	conversationBody    string
}

func (d *textDoer) Do(request *http.Request) (*http.Response, error) {
	body := []byte(nil)
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
	}
	switch request.URL.Path {
	case "/":
		return textResponse(`<html></html>`), nil
	case "/backend-api/sentinel/chat-requirements/prepare":
		return textResponse(`{"prepare_token":"prepare"}`), nil
	case "/backend-api/sentinel/chat-requirements/finalize":
		return textResponse(`{"token":"requirements","so_token":"so"}`), nil
	case "/backend-api/conversation":
		d.conversationRequest, d.conversationBody = request, string(body)
		return textResponse("data: {\"conversation_id\":\"conversation-1\",\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"Hello\"]}}}\n\ndata: {\"conversation_id\":\"conversation-1\",\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"Hello world\"]}}}\n\ndata: [DONE]\n\n"), nil
	default:
		return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
	}
}

func textResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}
}

func TestCompleteTextUsesRequirementsAndCollectsLatestSnapshot(t *testing.T) {
	doer := &textDoer{}
	client := newWithDoer(Config{AccessToken: "token"}, "https://chatgpt.com", doer)
	result, err := client.CompleteText(context.Background(), TextRequest{Model: "gpt-5", ThinkingEffort: "xhigh", Messages: []TextMessage{{Role: "system", Content: "be concise"}, {Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done || result.ConversationID != "conversation-1" || result.Text != "Hello world" {
		t.Fatalf("result=%+v", result)
	}
	if doer.conversationRequest == nil || doer.conversationRequest.Header.Get("Openai-Sentinel-Chat-Requirements-Token") != "requirements" || doer.conversationRequest.Header.Get("Openai-Sentinel-So-Token") != "so" || doer.conversationRequest.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers=%v", doer.conversationRequest.Header)
	}
	if !strings.Contains(doer.conversationBody, `"model":"gpt-5"`) || !strings.Contains(doer.conversationBody, `"thinking_effort":"extended"`) || !strings.Contains(doer.conversationBody, `"history_and_training_disabled":true`) || !strings.Contains(doer.conversationBody, `"force_use_sse":true`) {
		t.Fatalf("payload=%s", doer.conversationBody)
	}
}

func TestParseTextSSERejectsEmptyAssistantResult(t *testing.T) {
	if _, err := ParseTextSSE(context.Background(), strings.NewReader("data: {\"message\":{\"author\":{\"role\":\"user\"}}}\n\ndata: [DONE]\n\n")); err == nil {
		t.Fatal("ParseTextSSE succeeded without assistant text")
	}
}

func TestParseTextSSEEmitsSnapshotDeltas(t *testing.T) {
	var deltas []string
	stream := "data: {\"conversation_id\":\"c1\",\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"Hello\"]}}}\n\n" +
		"data: {\"conversation_id\":\"c1\",\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"Hello world\"]}}}\n\ndata: [DONE]\n\n"
	result, err := parseTextSSE(context.Background(), strings.NewReader(stream), func(delta TextDelta) error { deltas = append(deltas, delta.Text); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Hello world" || strings.Join(deltas, "") != "Hello world" || len(deltas) != 2 {
		t.Fatalf("result=%+v deltas=%q", result, deltas)
	}
}

func TestTextConversationPayloadUsesFileServicePointers(t *testing.T) {
	payload := textConversationPayload(TextRequest{Model: "auto"}, []preparedTextMessage{{Role: "user", Content: "describe", References: []ImageReference{{FileID: "file_123", FileName: "source.png", FileSize: 10, MIMEType: "image/png", Width: 2, Height: 3}}}})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(body)
	if !strings.Contains(encoded, `"content_type":"multimodal_text"`) || !strings.Contains(encoded, `"asset_pointer":"file-service://file_123"`) || !strings.Contains(encoded, `"attachments":[`) || !strings.Contains(encoded, `"describe"`) {
		t.Fatalf("payload=%s", encoded)
	}
}
