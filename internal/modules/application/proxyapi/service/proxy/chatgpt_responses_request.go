package proxy

import (
	"fmt"
	"sort"
	"strings"
)

// validateChatGPTResponsesFields runs before either executor is selected.
// The allowlist is intentional: new API semantics cannot silently disappear.
func validateChatGPTResponsesFields(body map[string]any) ([]string, *APIError) {
	if body == nil {
		return nil, invalidChatGPTResponseField("", "request body is required")
	}
	ignored := []string{}
	fields := make([]string, 0, len(body))
	for field := range body {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		value := body[field]
		switch field {
		case "model", "input":
			// Validated by routing and the bounded message mapper.
		case "stream":
			if _, ok := value.(bool); !ok {
				return nil, invalidChatGPTResponseField(field, "stream must be a boolean")
			}
		case "instructions":
			if value != nil {
				if _, ok := value.(string); !ok {
					return nil, invalidChatGPTResponseField(field, "instructions must be a string or null")
				}
			}
		case "background", "parallel_tool_calls", "store":
			if value == nil {
				continue
			}
			flag, ok := value.(bool)
			if !ok {
				return nil, invalidChatGPTResponseField(field, field+" must be a boolean")
			}
			if flag {
				return nil, unsupportedChatGPTWebFeature(field)
			}
		case "previous_response_id", "conversation", "prompt", "temperature", "top_p", "max_output_tokens", "service_tier", "response_format":
			if value != nil {
				return nil, unsupportedChatGPTWebFeature(field)
			}
		case "include":
			if value == nil {
				continue
			}
			list, ok := value.([]any)
			if !ok {
				return nil, invalidChatGPTResponseField(field, "include must be an array")
			}
			if len(list) != 0 {
				return nil, unsupportedChatGPTWebFeature(field)
			}
		case "truncation":
			if value != nil && value != "disabled" {
				return nil, unsupportedChatGPTWebFeature(field)
			}
		case "tools", "tool_choice":
			// Validated together by chatGPTWebSearchToolInvocation below.
		case "reasoning":
			if value == nil {
				continue
			}
			obj, ok := value.(map[string]any)
			if !ok {
				return nil, invalidChatGPTResponseField(field, "reasoning must be an object")
			}
			for _, key := range sortedChatGPTFields(obj) {
				val := obj[key]
				if val == nil && (key == "effort" || key == "summary") {
					continue
				}
				if key != "effort" {
					return nil, unsupportedChatGPTWebFeature("reasoning." + key)
				}
				effort, ok := val.(string)
				if !ok {
					return nil, invalidChatGPTResponseField("reasoning.effort", "reasoning.effort must be a string")
				}
				switch strings.ToLower(strings.TrimSpace(effort)) {
				case "low", "medium", "high", "xhigh", "extended":
				default:
					return nil, unsupportedChatGPTWebFeature("reasoning.effort")
				}
			}
		case "text":
			if value == nil {
				continue
			}
			obj, ok := value.(map[string]any)
			if !ok {
				return nil, invalidChatGPTResponseField(field, "text must be an object")
			}
			for _, key := range sortedChatGPTFields(obj) {
				if key != "format" {
					return nil, unsupportedChatGPTWebFeature("text." + key)
				}
				if obj[key] == nil {
					continue
				}
				format, ok := obj[key].(map[string]any)
				if !ok {
					return nil, invalidChatGPTResponseField("text.format", "text.format must be an object")
				}
				if typ, ok := format["type"].(string); !ok || typ != "text" {
					return nil, unsupportedChatGPTWebFeature("text.format")
				}
				if len(format) != 1 {
					return nil, unsupportedChatGPTWebFeature("text.format")
				}
			}
		case "metadata":
			if value == nil {
				continue
			}
			obj, ok := value.(map[string]any)
			if !ok || len(obj) > 16 {
				return nil, invalidChatGPTResponseField(field, "metadata must contain at most 16 string values")
			}
			for key, val := range obj {
				text, ok := val.(string)
				if !ok || len(key) > 64 || len(text) > 512 {
					return nil, invalidChatGPTResponseField(field, "metadata keys and string values exceed supported limits")
				}
			}
		case "user", "safety_identifier":
			if value == nil {
				continue
			}
			if _, ok := value.(string); !ok {
				return nil, invalidChatGPTResponseField(field, field+" must be a string")
			}
			ignored = append(ignored, field)
		default:
			return nil, unsupportedChatGPTWebFeature(field)
		}
	}
	return ignored, nil
}

func sortedChatGPTFields(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func invalidChatGPTResponseField(field, message string) *APIError {
	return &APIError{Code: ErrorCodeInvalidRequest, Message: message, Feature: field}
}

func rejectChatGPTFields(obj map[string]any, prefix string, allowed ...string) *APIError {
	for _, key := range sortedChatGPTFields(obj) {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return unsupportedChatGPTWebFeature(fmt.Sprintf("%s.%s", prefix, key))
		}
	}
	return nil
}
