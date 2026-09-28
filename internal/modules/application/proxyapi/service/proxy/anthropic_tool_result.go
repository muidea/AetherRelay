package proxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
)

func toolResultContentError(path, feature string) error {
	return &conversionLocationError{Path: path, Err: &conversionUnsupportedFeatureError{Feature: feature}}
}

// Text-only results retain their previous string encoding. Image results use
// Codex's native content array without moving images into unrelated messages.
func anthropicToolResultOutput(raw any, images bool) (any, error) {
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
		return anthropicToolResultText(raw)
	}
	return parts, nil
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
