package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	clientauth "aetherrelay/internal/pkg/aetherrelayclientauth"
)

const codexInputItemIDLimit = 64

type codexNormalizationOptions struct {
	compact             bool
	allowPreviousID     bool
	allowIncrementalOut bool
	responsesLite       bool
}

type codexRequestFeatures struct {
	RemoteCompactionV2 bool
	ResponsesLite      bool
}

var codexDropCompatibleHeaders = []string{
	"X-Codex-Turn-Metadata",
	"X-Codex-Turn-State",
	"Version",
}

func codexIgnoredHeaderNames(r *http.Request) []string {
	if r == nil || r.URL == nil {
		return nil
	}
	switch strings.TrimRight(r.URL.Path, "/") {
	case "/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages":
	default:
		return nil
	}
	ignored := make([]string, 0, len(codexDropCompatibleHeaders))
	for _, name := range codexDropCompatibleHeaders {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			ignored = append(ignored, name)
		}
	}
	return ignored
}

var codexDropCompatibleFields = []string{
	"max_output_tokens", "max_completion_tokens", "temperature", "top_p",
	"frequency_penalty", "presence_penalty", "user", "metadata",
	"prompt_cache_retention", "prompt_cache_options", "safety_identifier", "truncation",
}

// normalizeCodexRequest applies the deterministic client-side portion of
// CP-REQ-001..024 before an account is acquired.
func normalizeCodexRequest(raw []byte, compact bool) ([]byte, map[string]any, []string, error) {
	return normalizeCodexRequestWithOptions(raw, codexNormalizationOptions{compact: compact})
}

func normalizeCodexHTTPRequest(raw []byte, compact bool, headers http.Header) ([]byte, map[string]any, []string, codexRequestFeatures, error) {
	features, err := codexFeaturesFromHeaders(headers)
	if err != nil {
		return nil, nil, nil, codexRequestFeatures{}, err
	}
	features.ResponsesLite = features.ResponsesLite || rawCodexResponsesLite(raw)
	if compact && features.RemoteCompactionV2 {
		return nil, nil, nil, codexRequestFeatures{}, fmt.Errorf("remote_compaction_v2 is only valid on /v1/responses")
	}
	normalized, body, ignored, err := normalizeCodexRequestWithOptions(raw, codexNormalizationOptions{compact: compact, responsesLite: features.ResponsesLite})
	if err != nil {
		return nil, nil, nil, codexRequestFeatures{}, err
	}
	return normalized, body, ignored, features, nil
}

func rawCodexResponsesLite(raw []byte) bool {
	var envelope struct {
		ClientMetadata map[string]any `json:"client_metadata"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	return codexMetadataTrue(envelope.ClientMetadata["ws_request_header_x_openai_internal_codex_responses_lite"])
}

func codexFeaturesFromHeaders(headers http.Header) (codexRequestFeatures, error) {
	features := codexRequestFeatures{}
	if headers == nil {
		return features, nil
	}
	features.ResponsesLite = strings.EqualFold(strings.TrimSpace(headers.Get("X-OpenAI-Internal-Codex-Responses-Lite")), "true")
	for _, raw := range headers.Values("X-Codex-Beta-Features") {
		for _, token := range strings.Split(raw, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if token != "remote_compaction_v2" {
				return codexRequestFeatures{}, fmt.Errorf("X-Codex-Beta-Features value %q is not supported", token)
			}
			features.RemoteCompactionV2 = true
		}
	}
	return features, nil
}

func normalizeCodexRequestWithOptions(raw []byte, options codexNormalizationOptions) ([]byte, map[string]any, []string, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid JSON request body")
	}
	model, _ := body["model"].(string)
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		body["model"] = trimmed
	}
	if input, ok := body["input"].(string); ok {
		body["input"] = []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": input}},
		}}
	}
	if instructions, exists := body["instructions"]; !exists || instructions == nil {
		body["instructions"] = ""
	} else if _, ok := instructions.(string); !ok {
		return nil, nil, nil, fmt.Errorf("instructions must be a string")
	}
	if options.compact {
		delete(body, "stream")
		delete(body, "store")
		delete(body, "tool_choice")
	} else {
		body["stream"] = true
		body["store"] = false
		ensureCodexReasoningInclude(body)
	}
	convertLegacyCodexFunctions(body)
	sanitizeCodexToolParameterTypes(body["tools"])
	sanitizeCodexInputToolParameterTypes(body["input"])
	streamOptionsIgnored, err := normalizeCodexStreamOptions(body, options.compact)
	if err != nil {
		return nil, nil, nil, err
	}
	responsesLite, metadataIgnored, err := projectCodexClientMetadata(body)
	if err != nil {
		return nil, nil, nil, err
	}
	if responsesLite || options.responsesLite {
		body["parallel_tool_calls"] = false
		options.responsesLite = true
		if err := normalizeCodexResponsesLite(body); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := validateCodexRequest(body, options); err != nil {
		return nil, nil, nil, err
	}
	body["input"] = normalizeCodexInputItemIDs(body["input"])
	if options.compact {
		stripCodexCompactInputNamespaces(body["input"])
	}
	ignored := append([]string(nil), metadataIgnored...)
	ignored = append(ignored, streamOptionsIgnored...)
	for _, field := range codexDropCompatibleFields {
		if _, ok := body[field]; ok {
			delete(body, field)
			ignored = append(ignored, field)
		}
	}
	if tier, exists := body["service_tier"]; exists {
		value, ok := tier.(string)
		if !ok || strings.TrimSpace(value) != "priority" {
			delete(body, "service_tier")
			ignored = append(ignored, "service_tier")
		} else {
			body["service_tier"] = "priority"
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode normalized Codex request: %w", err)
	}
	return encoded, body, ignored, nil
}

// sanitizeCodexToolParameterTypes repairs the explicit null emitted by Codex
// Desktop for some built-in function schemas. A missing type remains missing,
// because adding one would narrow an otherwise valid JSON Schema.
func sanitizeCodexToolParameterTypes(value any) {
	tools, ok := value.([]any)
	if !ok {
		return
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"parameters"} {
			if parameters, ok := tool[key].(map[string]any); ok {
				if typ, exists := parameters["type"]; exists && typ == nil {
					parameters["type"] = "object"
				}
			}
		}
		if function, ok := tool["function"].(map[string]any); ok {
			if parameters, ok := function["parameters"].(map[string]any); ok {
				if typ, exists := parameters["type"]; exists && typ == nil {
					parameters["type"] = "object"
				}
			}
		}
		sanitizeCodexToolParameterTypes(tool["tools"])
	}
}

func sanitizeCodexInputToolParameterTypes(value any) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			sanitizeCodexToolParameterTypes(item["tools"])
		}
	}
}

func normalizeCodexStreamOptions(body map[string]any, compact bool) ([]string, error) {
	raw, exists := body["stream_options"]
	if !exists {
		return nil, nil
	}
	delete(body, "stream_options")
	if compact {
		return []string{"stream_options"}, nil
	}
	options, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stream_options must be an object")
	}
	ignored := make([]string, 0, len(options))
	for key := range options {
		if key != "reasoning_summary_delivery" {
			ignored = append(ignored, "stream_options."+key)
		}
	}
	if delivery, exists := options["reasoning_summary_delivery"]; exists {
		value, ok := delivery.(string)
		if !ok || strings.TrimSpace(value) != "sequential_cutoff" {
			return nil, fmt.Errorf("stream_options.reasoning_summary_delivery must be sequential_cutoff")
		}
		body["stream_options"] = map[string]any{"reasoning_summary_delivery": "sequential_cutoff"}
	}
	return ignored, nil
}

func normalizeCodexResponsesLite(body map[string]any) error {
	reasoning, exists := body["reasoning"]
	if !exists || reasoning == nil {
		body["reasoning"] = map[string]any{"context": "all_turns"}
	} else if object, ok := reasoning.(map[string]any); ok {
		object["context"] = "all_turns"
	} else {
		return fmt.Errorf("Responses Lite reasoning must be an object")
	}
	tools, exists := body["tools"]
	if !exists || tools == nil {
		return nil
	}
	list, ok := tools.([]any)
	if !ok {
		return fmt.Errorf("Responses Lite tools must be an array")
	}
	top := make([]any, 0, len(list))
	var moved []any
	for index, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("Responses Lite tools[%d] must be an object", index)
		}
		typ, _ := tool["type"].(string)
		switch strings.TrimSpace(typ) {
		case "function", "custom", "tool_search":
			top = append(top, raw)
		case "namespace":
			moved = append(moved, raw)
		default:
			return fmt.Errorf("Responses Lite tool type %q is not supported", typ)
		}
	}
	if len(moved) == 0 {
		return nil
	}
	input, err := appendCodexAdditionalTools(body["input"], moved)
	if err != nil {
		return err
	}
	body["input"] = input
	if len(top) == 0 {
		delete(body, "tools")
	} else {
		body["tools"] = top
	}
	return nil
}

func appendCodexAdditionalTools(value any, moved []any) ([]any, error) {
	items, ok := value.([]any)
	if !ok {
		if value == nil {
			items = []any{}
		} else {
			return nil, fmt.Errorf("Responses Lite namespace tools require array input")
		}
	}
	seen := map[string]any{}
	var target map[string]any
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "additional_tools" {
			continue
		}
		list, ok := item["tools"].([]any)
		if !ok && item["tools"] != nil {
			return nil, fmt.Errorf("Responses Lite additional_tools.tools must be an array")
		}
		if target == nil {
			target = item
		}
		for _, tool := range list {
			if err := rememberCodexLiteTool(seen, tool); err != nil {
				return nil, err
			}
		}
	}
	var additions []any
	for _, tool := range moved {
		key := codexLiteToolKey(tool)
		if previous, exists := seen[key]; exists {
			if !reflect.DeepEqual(previous, tool) {
				return nil, fmt.Errorf("Responses Lite additional_tools conflicts with %s", key)
			}
			continue
		}
		seen[key] = tool
		additions = append(additions, tool)
	}
	if target == nil {
		return append(items, map[string]any{"type": "additional_tools", "role": "developer", "tools": additions}), nil
	}
	existing, _ := target["tools"].([]any)
	target["tools"] = append(existing, additions...)
	return items, nil
}

func rememberCodexLiteTool(seen map[string]any, tool any) error {
	key := codexLiteToolKey(tool)
	if key == "" {
		return nil
	}
	if previous, exists := seen[key]; exists && !reflect.DeepEqual(previous, tool) {
		return fmt.Errorf("Responses Lite additional_tools contains conflicting %s", key)
	}
	seen[key] = tool
	return nil
}

func codexLiteToolKey(raw any) string {
	tool, _ := raw.(map[string]any)
	typ, _ := tool["type"].(string)
	name, _ := tool["name"].(string)
	if strings.TrimSpace(typ) == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	return strings.TrimSpace(typ) + "/" + strings.TrimSpace(name)
}

func ensureCodexReasoningInclude(body map[string]any) {
	if _, ok := body["reasoning"]; !ok {
		return
	}
	values, _ := body["include"].([]any)
	for _, value := range values {
		if text, _ := value.(string); text == "reasoning.encrypted_content" {
			return
		}
	}
	body["include"] = append(values, "reasoning.encrypted_content")
}

func convertLegacyCodexFunctions(body map[string]any) {
	if functions, ok := body["functions"].([]any); ok {
		tools := make([]any, 0, len(functions))
		for _, function := range functions {
			definition, ok := function.(map[string]any)
			if !ok {
				continue
			}
			tool := make(map[string]any, len(definition)+1)
			tool["type"] = "function"
			for key, value := range definition {
				tool[key] = value
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
		delete(body, "functions")
	}
	if choice, ok := body["function_call"]; ok {
		switch value := choice.(type) {
		case string:
			body["tool_choice"] = value
		case map[string]any:
			if name, _ := value["name"].(string); strings.TrimSpace(name) != "" {
				body["tool_choice"] = map[string]any{"type": "function", "name": name}
			}
		}
		delete(body, "function_call")
	}
}

func validateCodexRequest(body map[string]any, options codexNormalizationOptions) error {
	if previous, exists := body["previous_response_id"]; exists && previous != nil && strings.TrimSpace(fmt.Sprint(previous)) != "" {
		if !options.allowPreviousID {
			return fmt.Errorf("previous_response_id is not supported without an AetherRelay-owned response store")
		}
		value, ok := previous.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("previous_response_id must be a non-empty string")
		}
		body["previous_response_id"] = strings.TrimSpace(value)
	} else {
		delete(body, "previous_response_id")
	}
	if parallel, exists := body["parallel_tool_calls"]; exists {
		if _, ok := parallel.(bool); !ok {
			return fmt.Errorf("parallel_tool_calls must be a boolean")
		}
		tools, toolsOK := body["tools"].([]any)
		if options.compact || (!toolsOK || len(tools) == 0) && !options.responsesLite {
			delete(body, "parallel_tool_calls")
		}
	}
	if err := validateCodexTools(body["tools"], options.responsesLite); err != nil {
		return err
	}
	return validateCodexInput(body["input"], options.allowIncrementalOut)
}

func validateCodexTools(value any, allowToolSearch ...bool) error {
	if value == nil {
		return nil
	}
	tools, ok := value.([]any)
	if !ok {
		return fmt.Errorf("tools must be an array")
	}
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("tools[%d] must be an object", index)
		}
		typ, _ := tool["type"].(string)
		switch typ {
		case "function", "custom":
			if name, _ := tool["name"].(string); strings.TrimSpace(name) == "" {
				return fmt.Errorf("%s tool name is required", typ)
			}
		case "namespace":
			if name, _ := tool["name"].(string); strings.TrimSpace(name) == "" {
				return fmt.Errorf("namespace tool name is required")
			}
			if _, ok := tool["tools"].([]any); !ok {
				return fmt.Errorf("namespace tool %q tools must be an array", tool["name"])
			}
			if err := validateCodexTools(tool["tools"], allowToolSearch...); err != nil {
				return fmt.Errorf("namespace tool %q: %w", tool["name"], err)
			}
		case "tool_search":
			if len(allowToolSearch) == 0 || !allowToolSearch[0] {
				return fmt.Errorf("tool type %q is not supported by the Codex proxy", typ)
			}
		default:
			return fmt.Errorf("tool type %q is not supported by the Codex proxy", typ)
		}
	}
	return nil
}

func validateCodexInput(value any, allowIncrementalOutputs bool) error {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	functionCalls := map[string]struct{}{}
	customCalls := map[string]struct{}{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, exists := item["id"]; exists {
			if _, ok := id.(string); !ok {
				return fmt.Errorf("input item id is invalid")
			}
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "input_image":
			imageURL, _ := item["image_url"].(string)
			if strings.TrimSpace(imageURL) == "" {
				return fmt.Errorf("input_image image_url is required")
			}
		case "input_file", "computer_call", "computer_call_output", "image_generation_call":
			return fmt.Errorf("input item type %q is not supported by the Codex proxy", typ)
		case "function_call":
			if callID, _ := item["call_id"].(string); strings.TrimSpace(callID) != "" {
				functionCalls[strings.TrimSpace(callID)] = struct{}{}
			}
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			_, exists := functionCalls[strings.TrimSpace(callID)]
			if strings.TrimSpace(callID) == "" || !exists && !allowIncrementalOutputs {
				return fmt.Errorf("function_call_output references an unknown call_id")
			}
		case "custom_tool_call", "mcp_tool_call":
			if callID, _ := item["call_id"].(string); strings.TrimSpace(callID) != "" {
				customCalls[strings.TrimSpace(callID)] = struct{}{}
			}
		case "custom_tool_call_output", "mcp_tool_call_output":
			callID, _ := item["call_id"].(string)
			_, exists := customCalls[strings.TrimSpace(callID)]
			if strings.TrimSpace(callID) == "" || !exists && !allowIncrementalOutputs {
				return fmt.Errorf("%s references an unknown call_id", typ)
			}
		case "additional_tools":
			if err := validateCodexTools(item["tools"], true); err != nil {
				return fmt.Errorf("additional_tools: %w", err)
			}
		}
		if err := rejectCodexMultimodalContent(item["content"]); err != nil {
			return err
		}
	}
	return nil
}

func rejectCodexMultimodalContent(value any) error {
	parts, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := part["type"].(string)
		if typ != "" && typ != "input_text" && typ != "output_text" && typ != "text" && typ != "input_image" {
			return fmt.Errorf("content type %q is not supported by the Codex proxy", typ)
		}
		if typ == "input_image" {
			imageURL, _ := part["image_url"].(string)
			if strings.TrimSpace(imageURL) == "" {
				return fmt.Errorf("input_image image_url is required")
			}
		}
	}
	return nil
}

func normalizeCodexInputItemIDs(value any) any {
	items, _ := value.([]any)
	if items == nil {
		return value
	}
	occupied := make(map[string]struct{}, len(items))
	normalized := make([]any, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		original, ok := item["id"].(string)
		if ok && len([]rune(original)) > codexInputItemIDLimit && item["type"] == "reasoning" {
			if encrypted, _ := item["encrypted_content"].(string); encrypted != "" {
				continue
			}
		}
		if !ok || original == "" {
			normalized = append(normalized, raw)
			continue
		}
		prefix := codexInputItemPrefix(item)
		id := original
		if prefix != "" && !strings.HasPrefix(id, prefix+"_") {
			id = prefix + "_" + id
		}
		id = shortenCodexInputItemID(id, 0)
		for attempt := 1; ; attempt++ {
			if _, exists := occupied[id]; !exists {
				break
			}
			id = shortenCodexInputItemID(prefix+"_"+original, attempt)
		}
		occupied[id] = struct{}{}
		item["id"] = id
		normalized = append(normalized, raw)
	}
	return normalized
}

func codexInputItemPrefix(item map[string]any) string {
	switch item["type"] {
	case "message":
		return "msg"
	case "reasoning":
		return "rs"
	case "function_call":
		return "fc"
	case "custom_tool_call":
		return "ctc"
	case "custom_tool_call_output":
		return "ctco"
	default:
		return ""
	}
}

func shortenCodexInputItemID(id string, attempt int) string {
	runes := []rune(id)
	hashInput := id
	if attempt > 0 {
		hashInput += "\x00" + strconv.Itoa(attempt)
	}
	digest := sha256.Sum256([]byte(hashInput))
	suffix := "_" + hex.EncodeToString(digest[:8])
	if len(runes) <= codexInputItemIDLimit && attempt == 0 {
		return id
	}
	limit := codexInputItemIDLimit - len(suffix)
	if limit > len(runes) {
		limit = len(runes)
	}
	return string(runes[:limit]) + suffix
}

var codexClientMetadataAllowlist = map[string]struct{}{
	"x-codex-installation-id": {},
	"x-codex-turn-metadata":   {},
	"x-codex-window-id":       {},
	"ws_request_header_x_openai_internal_codex_responses_lite": {},
}

func projectCodexClientMetadata(body map[string]any) (bool, []string, error) {
	value, exists := body["client_metadata"]
	if !exists || value == nil {
		delete(body, "client_metadata")
		return false, nil, nil
	}
	metadata, ok := value.(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("client_metadata must be an object")
	}
	responsesLite := codexMetadataTrue(metadata["ws_request_header_x_openai_internal_codex_responses_lite"])
	ignored := make([]string, 0, len(metadata))
	for key := range metadata {
		if _, allowed := codexClientMetadataAllowlist[key]; !allowed {
			return false, nil, fmt.Errorf("client_metadata field %q is not supported", key)
		}
		ignored = append(ignored, "client_metadata."+key)
	}
	sort.Strings(ignored)
	delete(body, "client_metadata")
	return responsesLite, ignored, nil
}

func codexMetadataTrue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func stripCodexCompactInputNamespaces(value any) {
	items, _ := value.([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "function_call", "custom_tool_call", "mcp_tool_call":
			delete(item, "namespace")
		}
	}
}

// codexSessionHash implements CP-SCHED-002..003 without retaining the raw
// client session signal outside this request.
func codexSessionHash(r *http.Request, model string, body map[string]any) string {
	if r == nil {
		return ""
	}
	signal := ""
	for _, header := range []string{"Session-Id", "session_id", "conversation_id", "X-Session-Affinity", "X-Session-Id", "X-OpenCode-Session", "X-Conversation-ID"} {
		if signal = strings.TrimSpace(r.Header.Get(header)); signal != "" {
			break
		}
	}
	if signal == "" {
		if value, _ := body["prompt_cache_key"].(string); strings.TrimSpace(value) != "" {
			signal = strings.TrimSpace(value)
		}
	}
	if signal == "" {
		return ""
	}
	identity := clientauth.ClientIdentityFromContext(r.Context())
	digest := sha256.Sum256([]byte("aetherrelay:codex-session:v1\x00" + identity.KeyID + "\x00" + strings.TrimSpace(model) + "\x00" + signal))
	return hex.EncodeToString(digest[:])
}
