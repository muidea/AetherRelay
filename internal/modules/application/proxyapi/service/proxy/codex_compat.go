package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/codexresponses"
	clientauth "aetherrelay/internal/pkg/aetherrelayclientauth"
	"aetherrelay/internal/pkg/aetherrelaycodex"
)

const codexInputItemIDLimit = 64

const (
	codexRemoteCompactionV2Feature = "remote_compaction_v2"
	codexDefaultBetaFeatures       = codexRemoteCompactionV2Feature
	codexTurnStateHeader           = "X-Codex-Turn-State"
	codexTurnStateLimit            = 16 << 10
	codexBetaFeatureTokenLimit     = 32
	codexBetaFeatureValueLimit     = 4096
)

type codexNormalizationOptions struct {
	compact                bool
	allowPreviousID        bool
	allowIncrementalOut    bool
	responsesLite          bool
	allowBootstrap         bool
	allowHistoricalAnchors bool
}

type codexRequestFeatures struct {
	Diagnostics   codexresponses.Diagnostics
	BetaFeatures  string
	ResponsesLite bool
	TurnState     string
}

var codexDropCompatibleHeaders = []string{
	"X-Codex-Turn-Metadata",
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
// CP-REQ-001..035 before an account is acquired.
func normalizeCodexRequest(raw []byte, compact bool) ([]byte, map[string]any, []string, error) {
	return normalizeCodexRequestWithOptions(raw, codexNormalizationOptions{compact: compact})
}

func normalizeCodexHTTPRequest(raw []byte, compact bool, headers http.Header) ([]byte, map[string]any, []string, codexRequestFeatures, error) {
	features, err := codexFeaturesFromHeaders(headers)
	if err != nil {
		return nil, nil, nil, codexRequestFeatures{}, err
	}
	features.ResponsesLite = features.ResponsesLite || rawCodexResponsesLite(raw)
	metadata := headers.Get("X-Codex-Turn-Metadata")
	if metadata == "" {
		var source struct {
			ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
		}
		if json.Unmarshal(raw, &source) == nil {
			_ = json.Unmarshal(source.ClientMetadata["x-codex-turn-metadata"], &metadata)
		}
	}
	features.Diagnostics = codexresponses.ParseDiagnostics(metadata)
	nativeCompaction := rawCodexNativeCompactionV2(raw)
	if compact || nativeCompaction {
		features.BetaFeatures = ensureCodexBetaFeature(features.BetaFeatures, codexRemoteCompactionV2Feature)
	}
	normalized, body, ignored, err := normalizeCodexRequestWithOptions(raw, codexNormalizationOptions{compact: compact, responsesLite: features.ResponsesLite, allowBootstrap: !compact && !nativeCompaction})
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
	features := codexRequestFeatures{BetaFeatures: codexDefaultBetaFeatures}
	if headers == nil {
		return features, nil
	}
	features.ResponsesLite = strings.EqualFold(strings.TrimSpace(headers.Get("X-OpenAI-Internal-Codex-Responses-Lite")), "true")
	turnState := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	if len(turnState) > codexTurnStateLimit || strings.ContainsFunc(turnState, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return codexRequestFeatures{}, fmt.Errorf("X-Codex-Turn-State is invalid")
	}
	features.TurnState = turnState
	tokens := make([]string, 0, 4)
	seen := map[string]struct{}{}
	for _, raw := range headers.Values("X-Codex-Beta-Features") {
		for _, token := range strings.Split(raw, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if len(token) > 128 || strings.ContainsFunc(token, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
			}) {
				return codexRequestFeatures{}, fmt.Errorf("X-Codex-Beta-Features value %q is invalid", token)
			}
			if _, exists := seen[token]; exists {
				continue
			}
			projectedLength := len(token)
			if len(tokens) > 0 {
				projectedLength++
			}
			for _, existing := range tokens {
				projectedLength += len(existing)
			}
			if len(tokens) >= codexBetaFeatureTokenLimit || projectedLength > codexBetaFeatureValueLimit {
				return codexRequestFeatures{}, fmt.Errorf("X-Codex-Beta-Features is too large")
			}
			seen[token] = struct{}{}
			tokens = append(tokens, token)
		}
	}
	if len(tokens) > 0 {
		features.BetaFeatures = strings.Join(tokens, ",")
	}
	return features, nil
}

func ensureCodexBetaFeature(value, feature string) string {
	if codexBetaFeaturePresent(value, feature) {
		return value
	}
	if strings.TrimSpace(value) == "" {
		return feature
	}
	return strings.TrimSpace(value) + "," + feature
}

func codexBetaFeaturePresent(value, feature string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.TrimSpace(token) == feature {
			return true
		}
	}
	return false
}

func rawCodexNativeCompactionV2(raw []byte) bool {
	var envelope struct {
		Stream bool            `json:"stream"`
		Input  json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.Stream {
		return false
	}
	return rawCodexInputHasCompactionTrigger(envelope.Input)
}

func rawCodexInputHasCompactionTrigger(raw json.RawMessage) bool {
	var items []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return false
	}
	for _, item := range items {
		if item.Type == "compaction_trigger" {
			return true
		}
	}
	return false
}

func normalizeCodexRequestWithOptions(raw []byte, options codexNormalizationOptions) ([]byte, map[string]any, []string, error) {
	var body map[string]any
	if err := decodeCodexJSON(raw, &body); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid JSON request body")
	}
	if options.allowBootstrap {
		if !normalizeCodexCallOutputBootstrap(raw, body, isCodexAutomationBootstrap, false) {
			if normalizeCodexCallOutputBootstrap(raw, body, isCodexDelegationBootstrap, true) {
				options.allowHistoricalAnchors = true
				if previous, ok := body["previous_response_id"].(string); ok && strings.TrimSpace(previous) != "" {
					options.allowPreviousID = true
				}
			}
		}
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
	cacheBreakpointsIgnored := stripCodexInputPromptCacheBreakpoints(body["input"])
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
	aetherrelaycodex.NormalizeToolSchemas(body["tools"])
	normalizeCodexInputToolSchemas(body["input"])
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
	if cacheBreakpointsIgnored {
		ignored = append(ignored, "input[].content[].prompt_cache_breakpoint")
	}
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

// decodeCodexJSON preserves protocol integers across map/any normalization.
// Codex uses numeric sequence fields that can exceed JavaScript's safe integer
// range, so a float64 round trip is not acceptable here.
func decodeCodexJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("multiple JSON values are not supported")
	}
	return nil
}

// normalizeCodexCallOutputBootstrap implements CP-REQ-031 before the ordinary
// HTTP tool-history validator. It accepts only a fully recognized Codex
// bootstrap family; delegation may retain unambiguous historical anchors,
// while every ambiguous orphan output is rejected by validateCodexInput.
func normalizeCodexCallOutputBootstrap(raw []byte, body map[string]any, candidate func(map[string]any) bool, allowHistorical bool) bool {
	if candidate == nil || !hasUniqueCodexJSONMembers(raw) {
		return false
	}
	if previous, exists := body["previous_response_id"]; exists {
		value, ok := previous.(string)
		if !ok || !allowHistorical && strings.TrimSpace(value) != "" {
			return false
		}
	}
	input, ok := body["input"].([]any)
	if !ok {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		if candidate(item) {
			callIDValue, exists := item["call_id"]
			callID, isString := callIDValue.(string)
			if exists && (!isString || strings.TrimSpace(callID) != "") {
				return false
			}
			continue
		}
		if typ == "item_reference" {
			id, _ := item["id"].(string)
			if !allowHistorical || strings.TrimSpace(id) == "" {
				return false
			}
			continue
		}
		if strings.HasSuffix(typ, "_call") || isCodexCallOutputType(typ) {
			callID, _ := item["call_id"].(string)
			if !allowHistorical || strings.TrimSpace(callID) == "" {
				return false
			}
			continue
		}
	}
	changed := false
	for index, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !candidate(item) {
			continue
		}
		output, ok := item["output"].(string)
		if !ok {
			continue
		}
		input[index] = map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": output,
			}},
		}
		changed = true
	}
	return changed
}

func isCodexCallOutputType(typ string) bool {
	return strings.HasSuffix(typ, "_call_output") || typ == "tool_search_output"
}

func isCodexDelegationBootstrap(item map[string]any) bool {
	if codexStringField(item, "type") != "function_call_output" {
		return false
	}
	namespace := codexStringField(item, "namespace")
	name := codexStringField(item, "name")
	if (namespace != "codex_app" && namespace != "codex_tui") || (name != "create_thread" && name != "send_message_to_thread") {
		return false
	}
	output, ok := item["output"].(string)
	return ok && validCodexDelegationEnvelope(output)
}

func isCodexAutomationBootstrap(item map[string]any) bool {
	if codexStringField(item, "type") != "function_call_output" || codexStringField(item, "namespace") != "codex_app" || codexStringField(item, "name") != "automation_update" {
		return false
	}
	output, ok := item["output"].(string)
	return ok && validCodexAutomationBootstrap(output)
}

func codexStringField(item map[string]any, key string) string {
	value, _ := item[key].(string)
	return value
}

func validCodexAutomationBootstrap(value string) bool {
	if validCodexAutomationHeartbeat(value) {
		return true
	}
	normalized := strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsRune(normalized, '\r') {
		return false
	}
	lines := strings.Split(normalized, "\n")
	if len(lines) < 6 {
		return false
	}
	if _, ok := codexAutomationHeaderValue(lines[0], "Automation: "); !ok {
		return false
	}
	automationID, ok := codexAutomationHeaderValue(lines[1], "Automation ID: ")
	if !ok || !validCodexAutomationID(automationID) {
		return false
	}
	if lines[2] != "Automation memory: $CODEX_HOME/automations/"+automationID+"/memory.md" {
		return false
	}
	lastRun, ok := codexAutomationHeaderValue(lines[3], "Last run: ")
	if !ok || !validCodexAutomationLastRun(lastRun) || lines[4] != "" {
		return false
	}
	return strings.TrimSpace(strings.Join(lines[5:], "\n")) != ""
}

func validCodexAutomationHeartbeat(value string) bool {
	decoder := xml.NewDecoder(strings.NewReader(value))
	var rootSeen, idSeen bool
	var idText bytes.Buffer
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			id := idText.String()
			return rootSeen && idSeen && depth == 0 && strings.TrimSpace(id) == id && validCodexAutomationID(id)
		}
		if err != nil {
			return false
		}
		switch current := token.(type) {
		case xml.StartElement:
			depth++
			if current.Name.Space != "" || len(current.Attr) != 0 || depth > 2 {
				return false
			}
			if depth == 1 {
				if rootSeen || current.Name.Local != "heartbeat" {
					return false
				}
				rootSeen = true
			} else if idSeen || current.Name.Local != "automation_id" {
				return false
			}
			idSeen = depth == 2
		case xml.EndElement:
			if current.Name.Space != "" || depth == 2 && current.Name.Local != "automation_id" || depth == 1 && current.Name.Local != "heartbeat" {
				return false
			}
			depth--
			if depth < 0 {
				return false
			}
		case xml.CharData:
			if depth == 2 {
				_, _ = idText.Write(current)
			} else if len(bytes.TrimSpace(current)) != 0 {
				return false
			}
		case xml.Comment, xml.ProcInst, xml.Directive:
			return false
		}
	}
}

func normalizeCodexAgentMessages(value any) error {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(codexStringField(item, "type")) != "agent_message" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			return fmt.Errorf("input[%d].content must be an array", index)
		}
		for partIndex, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return fmt.Errorf("input[%d].content[%d] must be an object", index, partIndex)
			}
			if strings.TrimSpace(codexStringField(part, "type")) != "encrypted_content" {
				continue
			}
			encrypted, ok := part["encrypted_content"].(string)
			if !ok {
				return fmt.Errorf("input[%d].content[%d].encrypted_content must be a string", index, partIndex)
			}
			part["type"] = "input_text"
			part["text"] = encrypted
			delete(part, "encrypted_content")
		}
		item["type"] = "message"
		item["role"] = "user"
	}
	return nil
}

func codexAutomationHeaderValue(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(line, prefix)
	return value, value != "" && strings.TrimSpace(value) == value
}

func validCodexAutomationID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validCodexAutomationLastRun(value string) bool {
	if value == "never" {
		return true
	}
	separator := strings.LastIndex(value, " (")
	if separator <= 0 || !strings.HasSuffix(value, ")") {
		return false
	}
	runAt, err := time.Parse(time.RFC3339Nano, value[:separator])
	if err != nil {
		return false
	}
	epochMillis, err := strconv.ParseInt(value[separator+2:len(value)-1], 10, 64)
	return err == nil && runAt.UnixMilli() == epochMillis
}

func validCodexDelegationEnvelope(value string) bool {
	decoder := xml.NewDecoder(strings.NewReader(value))
	var rootSeen, sourceSeen, inputSeen bool
	var childName string
	var childText bytes.Buffer
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return rootSeen && depth == 0 && sourceSeen && inputSeen
		}
		if err != nil {
			return false
		}
		switch current := token.(type) {
		case xml.StartElement:
			depth++
			if current.Name.Space != "" || len(current.Attr) != 0 || depth == 1 && current.Name.Local != "codex_delegation" || depth > 2 {
				return false
			}
			if depth == 1 {
				if rootSeen {
					return false
				}
				rootSeen = true
				continue
			}
			if current.Name.Local != "source_thread_id" && current.Name.Local != "input" {
				return false
			}
			childName = current.Name.Local
			childText.Reset()
		case xml.EndElement:
			if current.Name.Space != "" {
				return false
			}
			if depth == 2 {
				if current.Name.Local != childName || strings.TrimSpace(childText.String()) == "" {
					return false
				}
				if childName == "source_thread_id" {
					if sourceSeen {
						return false
					}
					sourceSeen = true
				} else {
					if inputSeen {
						return false
					}
					inputSeen = true
				}
				childName = ""
			}
			depth--
			if depth < 0 {
				return false
			}
		case xml.CharData:
			if depth == 2 {
				_, _ = childText.Write(current)
			} else if len(bytes.TrimSpace(current)) != 0 {
				return false
			}
		case xml.Comment, xml.ProcInst, xml.Directive:
			return false
		}
	}
}

func hasUniqueCodexJSONMembers(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !consumeUniqueCodexJSONValue(decoder) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func consumeUniqueCodexJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delimiter {
	case '{':
		members := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok {
				return false
			}
			if _, duplicate := members[key]; duplicate {
				return false
			}
			members[key] = struct{}{}
			if !consumeUniqueCodexJSONValue(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for decoder.More() {
			if !consumeUniqueCodexJSONValue(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}

// stripCodexInputPromptCacheBreakpoints removes the per-content cache hint
// emitted by clients such as GitHub Copilot CLI. ChatGPT Codex rejects this
// field even though removing it leaves the standard Responses meaning intact.
func stripCodexInputPromptCacheBreakpoints(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := part["prompt_cache_breakpoint"]; exists {
				delete(part, "prompt_cache_breakpoint")
				changed = true
			}
		}
	}
	return changed
}

func normalizeCodexInputToolSchemas(value any) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			aetherrelaycodex.NormalizeToolSchemas(item["tools"])
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
		case "function", "custom", "tool_search", "web_search":
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
	if err := validateCodexTools(body["tools"]); err != nil {
		return err
	}
	if err := validateCodexWebSearchChoice(body["tool_choice"], body["tools"]); err != nil {
		return err
	}
	previous, _ := body["previous_response_id"].(string)
	return validateCodexInput(body["input"], options.allowIncrementalOut, options.allowHistoricalAnchors, options.allowHistoricalAnchors && strings.TrimSpace(previous) != "")
}

func validateCodexTools(value any) error {
	return validateCodexToolList(value, true)
}

func validateCodexToolList(value any, allowWebSearch bool) error {
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
			if err := validateCodexToolList(tool["tools"], false); err != nil {
				return fmt.Errorf("namespace tool %q: %w", tool["name"], err)
			}
		case "tool_search":
			if err := validateCodexToolSearch(tool); err != nil {
				return err
			}
		case "web_search":
			if !allowWebSearch {
				return fmt.Errorf("web_search must be declared in top-level tools")
			}
			if err := validateCodexWebSearchTool(tool); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tool type %q is not supported by the Codex proxy", typ)
		}
	}
	return nil
}

func validateCodexInput(value any, allowIncrementalOutputs bool, allowHistoricalAnchors ...bool) error {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	functionCalls := map[string]struct{}{}
	customCalls := map[string]struct{}{}
	historicalAnchors := len(allowHistoricalAnchors) > 0 && allowHistoricalAnchors[0]
	historicalContinuation := len(allowHistoricalAnchors) > 1 && allowHistoricalAnchors[1]
	historicalCalls := map[string]map[string]bool{}
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
		if historicalAnchors {
			if typ == "item_reference" {
				if !historicalContinuation {
					return fmt.Errorf("item_reference requires a historical continuation")
				}
				if id, _ := item["id"].(string); strings.TrimSpace(id) != "" {
					continue
				}
			}
			if strings.HasSuffix(typ, "_call") || isCodexCallOutputType(typ) {
				if callID, _ := item["call_id"].(string); strings.TrimSpace(callID) != "" {
					callID = strings.TrimSpace(callID)
					if isCodexCallOutputType(typ) {
						callType := strings.TrimSuffix(typ, "_output")
						if typ == "tool_search_output" {
							callType = "tool_search_call"
						}
						if !historicalContinuation && !historicalCalls[callType][callID] {
							return fmt.Errorf("%s references an unknown call_id", typ)
						}
					} else {
						if historicalCalls[typ] == nil {
							historicalCalls[typ] = map[string]bool{}
						}
						historicalCalls[typ][callID] = true
					}
					continue
				}
			}
		}
		switch typ {
		case "agent_message":
			if err := validateCodexAgentMessageContent(item["content"]); err != nil {
				return err
			}
			continue
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
			if err := validateCodexToolList(item["tools"], false); err != nil {
				return fmt.Errorf("additional_tools: %w", err)
			}
		}
		if err := rejectCodexMultimodalContent(item["content"]); err != nil {
			return err
		}
	}
	return nil
}

func validateCodexAgentMessageContent(value any) error {
	parts, ok := value.([]any)
	if !ok || len(parts) == 0 {
		return fmt.Errorf("agent_message content must be a non-empty array")
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("agent_message content block must be an object")
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "text":
			if _, ok := part["text"].(string); !ok {
				return fmt.Errorf("agent_message content text must be a string")
			}
		case "encrypted_content":
			if _, ok := part["encrypted_content"].(string); !ok {
				return fmt.Errorf("agent_message encrypted_content must be a string")
			}
		default:
			return fmt.Errorf("agent_message content type %q is not supported by the Codex proxy", typ)
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
	"parent_turn_id":           {},
	"root_turn_id":             {},
	"session_id":               {},
	"thread_id":                {},
	"turn_id":                  {},
	"x-codex-installation-id":  {},
	"x-codex-parent-thread-id": {},
	"x-codex-turn-metadata":    {},
	"x-codex-window-id":        {},
	"x-openai-subagent":        {},
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
	return codexSessionDigest(r, model, body, true)
}

// codexPromptCacheHash deliberately excludes routing-only signals. Claude
// Code's session id stabilizes account selection for /v1/messages but must not
// become an upstream prompt_cache_key.
func codexPromptCacheHash(r *http.Request, model string, body map[string]any) string {
	return codexSessionDigest(r, model, body, false)
}

func codexSessionDigest(r *http.Request, model string, body map[string]any, includeRoutingOnly bool) string {
	if r == nil {
		return ""
	}
	signal := ""
	if includeRoutingOnly && r.URL != nil && strings.TrimRight(r.URL.Path, "/") == "/v1/messages" {
		signal = strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id"))
	}
	for _, header := range []string{"Session-Id", "session_id", "conversation_id", "X-Session-Affinity", "X-Session-Id", "X-OpenCode-Session", "X-Conversation-ID"} {
		if signal != "" {
			break
		}
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
		signal = "default"
	}
	identity := clientauth.ClientIdentityFromContext(r.Context())
	digest := sha256.Sum256([]byte("aetherrelay:codex-session:v1\x00" + identity.KeyID + "\x00" + strings.TrimSpace(model) + "\x00" + signal))
	return hex.EncodeToString(digest[:])
}

func ensureCodexPromptCacheKey(encoded []byte, body map[string]any, sessionHash string) ([]byte, map[string]any, error) {
	if body == nil {
		body = map[string]any{}
	}
	if value, ok := body["prompt_cache_key"].(string); ok && strings.TrimSpace(value) != "" {
		return encoded, body, nil
	}
	if strings.TrimSpace(sessionHash) == "" {
		return encoded, body, nil
	}
	body["prompt_cache_key"] = strings.TrimSpace(sessionHash)
	updated, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Codex prompt cache key: %w", err)
	}
	return updated, body, nil
}
