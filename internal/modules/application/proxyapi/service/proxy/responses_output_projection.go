package proxy

import "fmt"

// Only these two output-item containers are known to be observational. Their
// contents are opaque and may grow independently of the execution protocol.
// Never strip unknown fields by prefix, or mutate the source used for archive.
func normalizeResponsesOutputItem(source map[string]any) (map[string]any, []string) {
	item := make(map[string]any, len(source))
	for key, value := range source {
		item[key] = value
	}
	var ignored []string
	for _, field := range []struct{ key, feature string }{
		{"internal_chat_message_metadata_passthrough", "internal_chat_message_metadata_passthrough"},
		{"metadata", "output_metadata"},
	} {
		if _, exists := item[field.key]; exists {
			delete(item, field.key)
			ignored = append(ignored, field.feature)
		}
	}
	return item, ignored
}

// Share field policy between incremental items and aggregated output. Required
// values and complete arguments are checked at the appropriate lifecycle stage.
func validateResponsesOutputItemFields(item map[string]any) error {
	switch item["type"] {
	case "message":
		return rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "role": {}, "status": {}, "content": {}, "phase": {}})
	case "function_call":
		if err := rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "call_id": {}, "name": {}, "arguments": {}, "status": {}}); err != nil {
			return err
		}
		for _, field := range []string{"call_id", "name", "arguments"} {
			if value, exists := item[field]; exists {
				text, ok := value.(string)
				if !ok || field != "arguments" && text == "" {
					return &conversionLocationError{Path: field, Err: fmt.Errorf("invalid function_call field")}
				}
			}
		}
	case "function_call_output":
		return rejectConversionFields(item, map[string]struct{}{"type": {}, "id": {}, "call_id": {}, "output": {}, "status": {}})
	}
	return nil // The caller handles reasoning and rejects unsupported item types.
}
