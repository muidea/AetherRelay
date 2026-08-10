package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	"github.com/gabriel-vasile/mimetype"
)

// writeArchiveRequest and writeArchiveResponse are the only body persistence
// boundary in proxyapi. Image bytes are always summarized before the recorder
// sees them, including when archive_full_content is enabled.
func (h *Handler) writeArchiveRequest(round *archive.Round, body []byte) error {
	if round == nil {
		return nil
	}
	return round.WriteRequest(sanitizeArchiveBody(body))
}

func (h *Handler) writeArchiveResponse(round *archive.Round, name string, body []byte) error {
	if round == nil {
		return nil
	}
	return round.WriteResponse(name, sanitizeArchiveBody(body))
}

func (h *Handler) createArchiveResponseWriter(round *archive.Round, name string) (io.WriteCloser, error) {
	if round == nil || !round.FullContent() {
		return archiveDiscardWriter{}, nil
	}
	// Mark the response path before the handler settles metadata. Close later
	// replaces this empty placeholder with the sanitized stream contents.
	if err := round.WriteResponse(name, nil); err != nil {
		return nil, err
	}
	return &sanitizingArchiveWriter{round: round, name: name}, nil
}

type archiveDiscardWriter struct{}

func (archiveDiscardWriter) Write(value []byte) (int, error) { return len(value), nil }
func (archiveDiscardWriter) Close() error                    { return nil }

// sanitizingArchiveWriter buffers one bounded response stream. The enclosing
// handler already enforces stream limits; buffering here ensures an SSE event
// with an image payload cannot bypass archive redaction through a raw file
// writer.
type sanitizingArchiveWriter struct {
	round *archive.Round
	name  string
	bytes.Buffer
}

func (w *sanitizingArchiveWriter) Close() error {
	if w == nil || w.round == nil {
		return nil
	}
	return w.round.WriteResponse(w.name, sanitizeArchiveBody(w.Bytes()))
}

func sanitizeArchiveBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var payload any
	if json.Unmarshal(body, &payload) == nil {
		if sanitized, changed := sanitizeArchiveValue(payload, ""); changed {
			if encoded, err := json.Marshal(sanitized); err == nil {
				if bytes.HasSuffix(body, []byte("\n")) {
					encoded = append(encoded, '\n')
				}
				return encoded
			}
		}
		return body
	}
	// SSE is not one JSON document. Redact each JSON data line independently.
	lines := strings.SplitAfter(string(body), "\n")
	changed := false
	for index, line := range lines {
		prefix, encoded, ok := strings.Cut(line, "data:")
		if !ok {
			continue
		}
		trailing := ""
		trimmed := strings.TrimSpace(encoded)
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		if strings.HasSuffix(encoded, "\n") {
			trailing = "\n"
		}
		var event any
		if json.Unmarshal([]byte(trimmed), &event) != nil {
			continue
		}
		if sanitized, didChange := sanitizeArchiveValue(event, ""); didChange {
			if marshaled, err := json.Marshal(sanitized); err == nil {
				lines[index] = prefix + "data: " + string(marshaled) + trailing
				changed = true
			}
		}
	}
	if changed {
		return []byte(strings.Join(lines, ""))
	}
	return body
}

func sanitizeArchiveValue(value any, key string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		result := make(map[string]any, len(typed))
		for field, child := range typed {
			sanitized, childChanged := sanitizeArchiveValue(child, field)
			result[field] = sanitized
			changed = changed || childChanged
		}
		return result, changed
	case []any:
		changed := false
		result := make([]any, len(typed))
		for index, child := range typed {
			sanitized, childChanged := sanitizeArchiveValue(child, key)
			result[index] = sanitized
			changed = changed || childChanged
		}
		return result, changed
	case string:
		if summary, ok := summarizeDataImageURL(typed); ok {
			return summary, true
		}
		if summary, ok := summarizeDataAttachmentURL(typed); ok {
			return summary, true
		}
		if isRawImageField(key) {
			if summary, ok := summarizeRawImageBase64(typed); ok {
				return summary, true
			}
		}
	}
	return value, false
}

func summarizeDataAttachmentURL(value string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(value)
	header, encoded, ok := strings.Cut(trimmed, ",")
	lowerHeader := strings.ToLower(header)
	if !ok || !strings.HasPrefix(lowerHeader, "data:") || !strings.Contains(lowerHeader, ";base64") {
		return nil, false
	}
	mimeType := strings.TrimPrefix(strings.Split(lowerHeader, ";")[0], "data:")
	decoded, err := decodeArchiveBase64(encoded)
	valueBytes := decoded
	if err != nil {
		valueBytes = []byte(trimmed)
	}
	digest := sha256.Sum256(valueBytes)
	summary := map[string]any{"redacted_attachment": true, "mime_type": mimeType, "sha256": hex.EncodeToString(digest[:])}
	if err == nil {
		summary["byte_count"] = len(decoded)
	} else {
		summary["encoded_byte_count"] = len(trimmed)
		summary["decode_error"] = "invalid_base64"
	}
	return summary, true
}

func isRawImageField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "b64_json", "image", "images", "mask", "masks":
		return true
	default:
		return false
	}
}

func summarizeDataImageURL(value string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(value)
	header, encoded, ok := strings.Cut(trimmed, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:image/") {
		return nil, false
	}
	mimeType := strings.TrimPrefix(strings.Split(strings.ToLower(header), ";")[0], "data:")
	decoded, err := decodeArchiveBase64(encoded)
	if err != nil {
		return imageArchiveSummary(mimeType, nil, []byte(trimmed), false), true
	}
	return imageArchiveSummary(mimeType, decoded, nil, true), true
}

func summarizeRawImageBase64(value string) (map[string]any, bool) {
	decoded, err := decodeArchiveBase64(strings.TrimSpace(value))
	if err != nil || len(decoded) == 0 {
		return nil, false
	}
	mimeType := strings.ToLower(mimetype.Detect(decoded).String())
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return imageArchiveSummary(mimeType, decoded, nil, true), true
	default:
		return nil, false
	}
}

func decodeArchiveBase64(value string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func imageArchiveSummary(mimeType string, decoded, raw []byte, decodedOK bool) map[string]any {
	value := decoded
	if !decodedOK {
		value = raw
	}
	digest := sha256.Sum256(value)
	summary := map[string]any{
		"redacted_image": true,
		"mime_type":      mimeType,
		"sha256":         hex.EncodeToString(digest[:]),
	}
	if decodedOK {
		summary["byte_count"] = len(decoded)
	} else {
		summary["encoded_byte_count"] = len(raw)
		// A malformed image data URI is still redacted rather than left on disk.
		summary["decode_error"] = "invalid_base64"
	}
	return summary
}
