package biz

import (
	"encoding/json"
	"fmt"
)

func forceNativeCompact(body []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	var input []json.RawMessage
	if raw := envelope["input"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("compact input must be an array")
		}
	}
	filtered := make([]json.RawMessage, 0, len(input)+1)
	for _, raw := range input {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "compaction_trigger" {
			continue
		}
		filtered = append(filtered, raw)
	}
	trigger, _ := json.Marshal(map[string]string{"type": "compaction_trigger"})
	input = append(filtered, trigger)
	rawInput, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	envelope["input"] = rawInput
	envelope["stream"] = json.RawMessage("true")
	envelope["store"] = json.RawMessage("false")
	delete(envelope, "tool_choice")
	return json.Marshal(envelope)
}

func nativeCompactResponse(body []byte) ([]byte, bool, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, false, err
	}
	var output []json.RawMessage
	if err := json.Unmarshal(response["output"], &output); err != nil {
		return nil, false, nil
	}
	found := false
	for _, raw := range output {
		if nativeCompactionItem(raw) {
			found = true
			break
		}
	}
	if !found {
		return nil, false, nil
	}
	object, _ := json.Marshal("response.compaction")
	response["object"] = object
	result, err := json.Marshal(response)
	return result, true, err
}

func nativeCompactionItem(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &item) == nil && (item.Type == "compaction" || item.Type == "compaction_summary")
}

func responseWithCompactionItems(body []byte, candidates map[int]json.RawMessage) []byte {
	if len(candidates) == 0 {
		return body
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	var output []json.RawMessage
	if raw := response["output"]; len(raw) > 0 && json.Unmarshal(raw, &output) != nil {
		return body
	}
	for _, item := range output {
		if nativeCompactionItem(item) {
			return body
		}
	}
	for _, item := range orderedOutputItems(candidates) {
		if nativeCompactionItem(item) {
			output = append(output, item)
		}
	}
	rawOutput, err := json.Marshal(output)
	if err != nil {
		return body
	}
	response["output"] = rawOutput
	result, err := json.Marshal(response)
	if err != nil {
		return body
	}
	return result
}
