package proxy

import (
	"strings"
	"testing"
)

func TestSanitizeArchiveBodyRedactsImageDataAndB64JSON(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLz4QAAAABJRU5ErkJggg=="
	body := []byte(`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"` + dataURI + `"}}]}],"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLz4QAAAABJRU5ErkJggg=="}]}`)
	sanitized := string(sanitizeArchiveBody(body))
	if strings.Contains(sanitized, "iVBORw0KGgo") || !strings.Contains(sanitized, `"redacted_image":true`) || !strings.Contains(sanitized, `"sha256"`) {
		t.Fatalf("sanitized=%s", sanitized)
	}
	stream := []byte("event: response.output_item.added\ndata: {\"image\":\"" + dataURI + "\"}\n\n")
	if got := string(sanitizeArchiveBody(stream)); strings.Contains(got, "iVBORw0KGgo") || !strings.Contains(got, `"redacted_image":true`) {
		t.Fatalf("sanitized SSE=%s", got)
	}
}

func TestSanitizeArchiveBodyRedactsFileData(t *testing.T) {
	body := []byte(`{"input":[{"content":[{"type":"input_file","filename":"notes.md","file_data":"data:text/markdown;base64,IyBzZWNyZXQ="}]}]}`)
	sanitized := string(sanitizeArchiveBody(body))
	if strings.Contains(sanitized, "IyBzZWNyZXQ=") || !strings.Contains(sanitized, `"redacted_attachment":true`) || !strings.Contains(sanitized, `"mime_type":"text/markdown"`) {
		t.Fatalf("sanitized=%s", sanitized)
	}
}

func TestSanitizeArchiveBodyRedactsSignedImageCapability(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://relay.test/images/test-client/2026/09/10/result.png?expires=1789000000&key_id=test-client&signature=replayable-secret"}]}`)
	sanitized := string(sanitizeArchiveBody(body))
	if strings.Contains(sanitized, "replayable-secret") || !strings.Contains(sanitized, "REDACTED") {
		t.Fatalf("sanitized=%s", sanitized)
	}
	if !strings.Contains(sanitized, "key_id=test-client") || !strings.Contains(sanitized, "/images/test-client/") {
		t.Fatalf("archive lost non-secret image reference: %s", sanitized)
	}
	external := []byte(`{"data":[{"url":"https://provider.test/result.png?signature=provider-value"}]}`)
	if got := string(sanitizeArchiveBody(external)); got != string(external) {
		t.Fatalf("external provider URL was changed: %s", got)
	}
}
