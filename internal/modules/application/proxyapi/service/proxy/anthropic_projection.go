package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

// Project only known non-execution fields. Never mutate the source request or
// traverse tool schemas and tool arguments as if they were protocol envelopes.
func projectAnthropicAnnotations(body map[string]any) (map[string]any, []string, error) {
	ignored := []string{}
	copyObject := func(src map[string]any) map[string]any {
		dst := make(map[string]any, len(src))
		for k, v := range src {
			dst[k] = v
		}
		return dst
	}
	out := copyObject(body)
	if raw, exists := body["metadata"]; exists {
		if raw != nil {
			metadata, ok := raw.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("metadata must be an object")
			}
			for key, value := range metadata {
				if key != "user_id" {
					return nil, nil, fmt.Errorf("metadata.%s", key)
				}
				if value != nil {
					id, ok := value.(string)
					if !ok || len(id) > 4096 {
						return nil, nil, fmt.Errorf("metadata.user_id")
					}
				}
			}
		}
		delete(out, "metadata")
		ignored = append(ignored, "metadata.user_id")
	}
	blocks := func(raw any) (any, error) {
		items, ok := raw.([]any)
		if !ok {
			return raw, nil
		}
		result := append([]any(nil), items...)
		for i, entry := range items {
			block, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			cache, exists := block["cache_control"]
			if !exists {
				continue
			}
			control, ok := cache.(map[string]any)
			if !ok || control["type"] != "ephemeral" {
				return nil, fmt.Errorf("cache_control.type")
			}
			for key, value := range control {
				if key == "type" {
					continue
				}
				if key != "ttl" || (value != "5m" && value != "1h") {
					return nil, fmt.Errorf("cache_control.%s", key)
				}
			}
			clone := copyObject(block)
			delete(clone, "cache_control")
			result[i] = clone
			ignored = append(ignored, "cache_control")
		}
		return result, nil
	}
	var err error
	if out["system"], err = blocks(body["system"]); err != nil {
		return nil, nil, err
	}
	if messages, ok := body["messages"].([]any); ok {
		result := append([]any(nil), messages...)
		for i, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			clone := copyObject(message)
			if clone["content"], err = blocks(message["content"]); err != nil {
				return nil, nil, err
			}
			result[i] = clone
		}
		out["messages"] = result
	}
	return out, uniqueSortedFeatures(ignored), nil
}

// This is a client-observed envelope, not the universal Anthropic user_id
// contract. Unknown/plain user IDs remain opaque and never become sessions.
func anthropicEmbeddedSession(body map[string]any) string {
	metadata, _ := body["metadata"].(map[string]any)
	value, _ := metadata["user_id"].(string)
	var identity map[string]any
	if len(value) > 4096 || json.Unmarshal([]byte(value), &identity) != nil {
		return ""
	}
	if len(identity) != 3 {
		return ""
	}
	device, d := identity["device_id"].(string)
	_, a := identity["account_uuid"].(string)
	session, s := identity["session_id"].(string)
	if !d || !a || !s || device == "" || len(session) > 256 {
		return ""
	}
	return strings.TrimSpace(session)
}

func anthropicTargetReasoning(body map[string]any, metadata config.ModelMetadata, capability config.ConversionCapability) (config.ConversionCapability, error) {
	if _, present := body["thinking"]; !present {
		output, _ := body["output_config"].(map[string]any)
		if _, hasEffort := output["effort"]; !hasEffort {
			return capability, nil
		}
	}
	if !metadata.ReasoningDeclared || !metadata.ReasoningSupported {
		return capability, fmt.Errorf("thinking")
	}
	effort := metadata.ReasoningDefaultEffort
	if output, ok := body["output_config"].(map[string]any); ok {
		if raw, present := output["effort"]; present {
			value, ok := raw.(string)
			if !ok {
				return capability, fmt.Errorf("output_config.effort")
			}
			effort = value
		}
	}
	for _, allowed := range metadata.ReasoningEfforts {
		if effort != "" && effort == allowed {
			capability.Reasoning = true
			capability.ReasoningAdapter = config.ReasoningAdapterAnthropicToResponsesEffort
			capability.ReasoningTargetEffort = effort
			return capability, nil
		}
	}
	return capability, fmt.Errorf("output_config.effort is unsupported by target model")
}
