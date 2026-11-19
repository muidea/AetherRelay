package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func toolResultContentError(path, feature string) error {
	return &conversionLocationError{Path: path, Err: &conversionUnsupportedFeatureError{Feature: feature}}
}

// Text-only results retain their previous string encoding. Image results use
// Codex's native content array without moving images into unrelated messages.
func anthropicToolResultOutput(raw any, images bool) (any, error) {
	return anthropicToolResultOutputWithDiscovery(raw, images, nil)
}

func anthropicToolResultOutputWithDiscovery(raw any, images bool, codexTools *codexAnthropicToolRegistry) (any, error) {
	blocks, ok := raw.([]any)
	if !ok {
		return anthropicToolResultText(raw)
	}
	parts := make([]map[string]any, 0, len(blocks))
	hasImage := false
	for i, rawBlock := range blocks {
		path := fmt.Sprintf("content[%d]", i)
		block, ok := rawBlock.(map[string]any)
		if !ok {
			return nil, toolResultContentError(path, "tool_result.content")
		}
		switch block["type"] {
		case "text":
			if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "text": {}}); err != nil {
				return nil, toolResultContentError(path, "tool_result.content.text")
			}
			text, ok := block["text"].(string)
			if !ok {
				return nil, toolResultContentError(path+".text", "tool_result.content.text")
			}
			parts = append(parts, map[string]any{"type": "input_text", "text": text})
		case "document":
			docParts, err := anthropicDocumentParts(block, images)
			if err != nil {
				var located *conversionLocationError
				if errors.As(err, &located) {
					path += "." + located.Path
				}
				return nil, &conversionLocationError{Path: path, Err: &conversionUnsupportedFeatureError{Feature: "tool_result.document"}}
			}
			for _, part := range docParts {
				if part["type"] == "input_image" {
					hasImage = true
				}
				parts = append(parts, part)
			}
		case "tool_reference":
			if codexTools == nil {
				return nil, toolResultContentError(path+".type", "tool_result.content.type")
			}
			refText, err := codexTools.recordReference(block)
			if err != nil {
				return nil, toolResultContentError(path, "tool_result.tool_reference")
			}
			parts = append(parts, map[string]any{"type": "input_text", "text": refText})
		case "image":
			if !images {
				return nil, toolResultContentError(path, "tool_result.image")
			}
			image, err := anthropicToolResultImage(block)
			if err != nil {
				var located *conversionLocationError
				if errors.As(err, &located) {
					path += "." + located.Path
				}
				return nil, &conversionLocationError{Path: path, Err: err}
			}
			parts = append(parts, image)
			hasImage = true
		default:
			return nil, toolResultContentError(path+".type", "tool_result.content.type")
		}
	}
	if !hasImage {
		return anthropicToolResultPartsText(parts), nil
	}
	return parts, nil
}

func anthropicToolResultPartsText(parts []map[string]any) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func anthropicDocumentHeader(block map[string]any) (string, error) {
	var title, contextText string
	if raw, exists := block["title"]; exists {
		value, ok := raw.(string)
		if !ok {
			return "", &conversionLocationError{Path: "title", Err: fmt.Errorf("document.title")}
		}
		title = strings.TrimSpace(value)
	}
	if raw, exists := block["context"]; exists {
		value, ok := raw.(string)
		if !ok {
			return "", &conversionLocationError{Path: "context", Err: fmt.Errorf("document.context")}
		}
		contextText = strings.TrimSpace(value)
	}
	if title == "" && contextText == "" {
		return "", nil
	}
	var out strings.Builder
	if title != "" {
		out.WriteString("Document: ")
		out.WriteString(title)
	}
	if contextText != "" {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString("Context: ")
		out.WriteString(contextText)
	}
	return out.String(), nil
}

// anthropicDocumentParts converts Anthropic plain-text and content document
// blocks into Responses input parts while rejecting unmodeled binary/URL sources.
func anthropicDocumentParts(block map[string]any, allowImages bool) ([]map[string]any, error) {
	if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "source": {}, "title": {}, "context": {}}); err != nil {
		return nil, err
	}
	header, err := anthropicDocumentHeader(block)
	if err != nil {
		return nil, err
	}
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, &conversionLocationError{Path: "source", Err: fmt.Errorf("document.source")}
	}
	sourceType, _ := source["type"].(string)
	switch sourceType {
	case "text":
		if err := rejectConversionFields(source, map[string]struct{}{"type": {}, "media_type": {}, "data": {}}); err != nil {
			return nil, &conversionLocationError{Path: "source", Err: err}
		}
		if rawMedia, exists := source["media_type"]; exists {
			media, ok := rawMedia.(string)
			if !ok || (strings.TrimSpace(media) != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(media)), "text/")) {
				return nil, &conversionLocationError{Path: "source.media_type", Err: fmt.Errorf("document.source.media_type")}
			}
		}
		data, ok := source["data"].(string)
		if !ok {
			return nil, &conversionLocationError{Path: "source.data", Err: fmt.Errorf("document.source.data")}
		}
		text := data
		if header != "" && strings.TrimSpace(data) != "" {
			text = header + "\n" + data
		}
		return []map[string]any{{"type": "input_text", "text": text}}, nil
	case "content":
		if err := rejectConversionFields(source, map[string]struct{}{"type": {}, "content": {}}); err != nil {
			return nil, &conversionLocationError{Path: "source", Err: err}
		}
		rawContent, exists := source["content"]
		if !exists {
			return nil, &conversionLocationError{Path: "source.content", Err: fmt.Errorf("document.source.content")}
		}
		var parts []map[string]any
		emitText := func(text string) {
			if header != "" && len(parts) == 0 && strings.TrimSpace(text) != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": header})
			}
			parts = append(parts, map[string]any{"type": "input_text", "text": text})
		}
		switch typed := rawContent.(type) {
		case string:
			emitText(typed)
		case []any:
			for idx, rawItem := range typed {
				itemPath := fmt.Sprintf("source.content[%d]", idx)
				if text, ok := rawItem.(string); ok {
					emitText(text)
					continue
				}
				item, ok := rawItem.(map[string]any)
				if !ok {
					return nil, &conversionLocationError{Path: itemPath, Err: fmt.Errorf("document.source.content")}
				}
				switch item["type"] {
				case "text":
					if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "text": {}}); err != nil {
						return nil, &conversionLocationError{Path: itemPath, Err: err}
					}
					text, ok := item["text"].(string)
					if !ok {
						return nil, &conversionLocationError{Path: itemPath + ".text", Err: fmt.Errorf("document.source.content.text")}
					}
					emitText(text)
				case "image":
					if !allowImages {
						return nil, &conversionLocationError{Path: itemPath, Err: &conversionUnsupportedFeatureError{Feature: "document.image"}}
					}
					image, err := anthropicToolResultImage(item)
					if err != nil {
						return nil, &conversionLocationError{Path: itemPath, Err: err}
					}
					if header != "" && len(parts) == 0 {
						parts = append(parts, map[string]any{"type": "input_text", "text": header})
					}
					parts = append(parts, image)
				default:
					return nil, &conversionLocationError{Path: itemPath + ".type", Err: fmt.Errorf("document.source.content.type")}
				}
			}
		default:
			return nil, &conversionLocationError{Path: "source.content", Err: fmt.Errorf("document.source.content")}
		}
		return parts, nil
	default:
		return nil, &conversionLocationError{Path: "source.type", Err: fmt.Errorf("document.source.type")}
	}
}

func anthropicDocumentText(block map[string]any) (string, error) {
	parts, err := anthropicDocumentParts(block, false)
	if err != nil {
		return "", err
	}
	return anthropicToolResultPartsText(parts), nil
}

// Only inline images are supported here; the relay does not fetch image URLs.
func anthropicToolResultImage(block map[string]any) (map[string]any, error) {
	fail := func(path string) (map[string]any, error) {
		return nil, toolResultContentError(path, "tool_result.image")
	}
	if err := rejectConversionFields(block, map[string]struct{}{"type": {}, "source": {}}); err != nil {
		return fail("source")
	}
	source, ok := block["source"].(map[string]any)
	if !ok {
		return fail("source")
	}
	if source["type"] != "base64" {
		return fail("source.type")
	}
	if err := rejectConversionFields(source, map[string]struct{}{"type": {}, "media_type": {}, "data": {}}); err != nil {
		return fail("source")
	}
	mediaType, _ := source["media_type"].(string)
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return fail("source.media_type")
	}
	data, ok := source["data"].(string)
	if !ok || data == "" || len(data) > maxConversionToolArgumentBytes {
		return fail("source.data")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil || len(decoded) == 0 {
		return fail("source.data")
	}
	if http.DetectContentType(decoded) != mediaType {
		return fail("source.media_type")
	}
	return map[string]any{"type": "input_image", "image_url": "data:" + mediaType + ";base64," + data}, nil
}
