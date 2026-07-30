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
