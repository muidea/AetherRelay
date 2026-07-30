package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// chatGPTWebChatCompatibility validates the OpenAI Chat Completions subset
// that can be represented by a ChatGPT Web conversation. It deliberately
// keeps common sampling and tracing knobs compatible: the Web upstream cannot
// honour them, but recording their names in archive metadata makes that
// degradation observable instead of silently dropping them. Features whose
// semantics would change the result (tools, structured output, multi-choice)
// are rejected before an account or upstream request is used.
func chatGPTWebChatCompatibility(body map[string]any) ([]string, *APIError) {
	if body == nil {
		return nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "request body is required"}
	}
	for _, feature := range []string{
		"tools", "tool_choice", "parallel_tool_calls", "functions", "function_call",
		"response_format", "logprobs", "top_logprobs",
	} {
		if _, present := body[feature]; present {
			return nil, unsupportedChatGPTWebFeature(feature)
		}
	}
	if n, present := body["n"]; present && !isOne(n) {
		return nil, unsupportedChatGPTWebFeature("n")
	}
	if rawMessages, present := body["messages"]; present {
		messages, ok := rawMessages.([]any)
		if !ok {
			return nil, &APIError{Code: ErrorCodeInvalidRequest, Message: "messages must be an array", Feature: "messages"}
		}
		for index, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok {
				continue // chatGPTTextRequest returns the precise structural error.
			}
			for _, feature := range []string{"tool_calls", "function_call"} {
				if _, present := message[feature]; present {
					return nil, unsupportedChatGPTWebFeature(fmt.Sprintf("messages[%d].%s", index, feature))
				}
			}
		}
	}

	known := map[string]struct{}{
		"model": {}, "messages": {}, "stream": {}, "reasoning_effort": {},
		"tools": {}, "tool_choice": {}, "parallel_tool_calls": {}, "functions": {}, "function_call": {},
		"response_format": {}, "logprobs": {}, "top_logprobs": {}, "n": {},
	}
	ignored := make([]string, 0, len(body))
	for field := range body {
		if _, handled := known[field]; handled {
			continue
		}
		ignored = append(ignored, field)
	}
	// These widely-used OpenAI request controls are intentionally accepted but
	// not forwarded because ChatGPT Web has no equivalent deterministic knob.
	for _, field := range []string{
		"temperature", "top_p", "max_tokens", "max_completion_tokens", "stop",
		"presence_penalty", "frequency_penalty", "seed", "user", "metadata",
		"store", "stream_options", "service_tier", "verbosity", "prediction",
	} {
		if _, present := body[field]; present {
			ignored = append(ignored, field)
		}
	}
	return uniqueSortedFeatures(ignored), nil
}

func unsupportedChatGPTWebFeature(feature string) *APIError {
	return &APIError{
		Code:    ErrorCodeConversionUnsupported,
		Message: fmt.Sprintf("chatgpt web does not support %s", feature),
		Feature: feature,
	}
}

func isOne(value any) bool {
	switch v := value.(type) {
	case float64:
		return v == 1
	case float32:
		return v == 1
	case int:
		return v == 1
	case int64:
		return v == 1
	case json.Number:
		return strings.TrimSpace(string(v)) == "1"
	default:
		return false
	}
}

func uniqueSortedFeatures(features []string) []string {
	if len(features) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(features))
	result := make([]string, 0, len(features))
	for _, feature := range features {
		feature = strings.TrimSpace(feature)
		if feature == "" {
			continue
		}
		if _, exists := seen[feature]; exists {
			continue
		}
		seen[feature] = struct{}{}
		result = append(result, feature)
	}
	sort.Strings(result)
	return result
}
