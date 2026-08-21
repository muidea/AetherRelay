package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

const maxCodexWebsocketTurnMigrations = 2

type codexWebsocketReplayState struct {
	items  []json.RawMessage
	exists bool
	limit  int64
}

type codexWebsocketTurnReplay struct {
	items  []json.RawMessage
	exists bool
	safe   bool
}

func newCodexWebsocketReplayState(limit int64) *codexWebsocketReplayState {
	return &codexWebsocketReplayState{limit: limit}
}

// prepare derives the complete input sequence needed if this turn must move to
// another account. It does not mutate committed history until the turn ends
// successfully.
func (s *codexWebsocketReplayState) prepare(payload []byte) (codexWebsocketTurnReplay, error) {
	current, currentExists, err := codexWebsocketInputSequence(payload)
	if err != nil {
		return codexWebsocketTurnReplay{}, err
	}
	needsHistory, err := codexWebsocketPayloadNeedsHistory(payload, current)
	if err != nil {
		return codexWebsocketTurnReplay{}, err
	}
	full, exists := cloneCodexWebsocketRawItems(current), currentExists
	if needsHistory && s != nil && s.exists {
		if codexWebsocketRawItemsHavePrefix(current, s.items) {
			full = cloneCodexWebsocketRawItems(current)
		} else {
			full = append(cloneCodexWebsocketRawItems(s.items), cloneCodexWebsocketRawItems(current)...)
		}
		exists = true
	}
	turn := codexWebsocketTurnReplay{items: full, exists: exists, safe: exists}
	if needsHistory && (s == nil || !s.exists) && codexWebsocketRawItemsHaveToolOutput(current) {
		turn.safe = false
	}
	limit := int64(0)
	if s != nil {
		limit = s.limit
	}
	if turn.safe && !codexWebsocketRawItemsWithinLimit(turn.items, limit) {
		turn.safe = false
	}
	return turn, nil
}

func (s *codexWebsocketReplayState) commit(turn codexWebsocketTurnReplay, output []json.RawMessage) {
	if s == nil {
		return
	}
	items := append(cloneCodexWebsocketRawItems(turn.items), cloneCodexWebsocketRawItems(output)...)
	if !codexWebsocketRawItemsWithinLimit(items, s.limit) {
		s.items = nil
		s.exists = false
		return
	}
	s.items = items
	s.exists = turn.exists || len(output) > 0
}

func buildCodexWebsocketRetryPayload(payload []byte, turn codexWebsocketTurnReplay, limit int64) ([]byte, bool, error) {
	if !turn.safe || !turn.exists {
		return nil, false, nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, false, err
	}
	encodedInput, err := json.Marshal(turn.items)
	if err != nil {
		return nil, false, err
	}
	var input []any
	if err := json.Unmarshal(encodedInput, &input); err != nil {
		return nil, false, err
	}
	body["input"] = input
	delete(body, "previous_response_id")
	if err := validateCodexInput(body["input"], false); err != nil {
		return nil, false, nil
	}
	retry, err := json.Marshal(body)
	if err != nil {
		return nil, false, err
	}
	if limit > 0 && int64(len(retry)) > limit {
		return nil, false, nil
	}
	return retry, true, nil
}

func codexWebsocketInputSequence(payload []byte) ([]json.RawMessage, bool, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, false, err
	}
	raw, exists := body["input"]
	if !exists {
		return nil, false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true, nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, true, err
		}
		return cloneCodexWebsocketRawItems(items), true, nil
	}
	return []json.RawMessage{append(json.RawMessage(nil), trimmed...)}, true, nil
}

func codexWebsocketPayloadNeedsHistory(payload []byte, current []json.RawMessage) (bool, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return false, err
	}
	var previous string
	_ = json.Unmarshal(body["previous_response_id"], &previous)
	return strings.TrimSpace(previous) != "" || codexWebsocketRawItemsHaveToolOutput(current), nil
}

func codexWebsocketRawItemsHaveToolOutput(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) == nil {
			switch item.Type {
			case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output":
				return true
			}
		}
	}
	return false
}

func codexWebsocketRawItemsHavePrefix(items, prefix []json.RawMessage) bool {
	if len(prefix) == 0 {
		return true
	}
	if len(items) < len(prefix) {
		return false
	}
	for index := range prefix {
		var left, right any
		if json.Unmarshal(items[index], &left) != nil || json.Unmarshal(prefix[index], &right) != nil || !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}

func codexWebsocketRawItemsWithinLimit(items []json.RawMessage, limit int64) bool {
	if limit <= 0 {
		return true
	}
	encoded, err := json.Marshal(items)
	return err == nil && int64(len(encoded)) <= limit
}

func cloneCodexWebsocketRawItems(items []json.RawMessage) []json.RawMessage {
	if items == nil {
		return nil
	}
	result := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		result = append(result, append(json.RawMessage(nil), item...))
	}
	return result
}

type codexWebsocketOutputCollector struct {
	items []json.RawMessage
	seen  map[string]struct{}
}

func (c *codexWebsocketOutputCollector) addEvent(payload []byte) {
	var event struct {
		Type     string          `json:"type"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "response.output_item.done":
		c.add(event.Item)
	case "response.completed", "response.done", "response.incomplete":
		for _, item := range event.Response.Output {
			c.add(item)
		}
	}
}

func (c *codexWebsocketOutputCollector) add(raw json.RawMessage) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}
	var item struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(trimmed, &item) != nil || strings.TrimSpace(item.Type) == "" {
		return
	}
	key := strings.TrimSpace(item.ID)
	if key == "" {
		key = strings.TrimSpace(item.CallID)
	}
	if key == "" {
		key = fmt.Sprintf("%s:%s", item.Type, trimmed)
	}
	if c.seen == nil {
		c.seen = map[string]struct{}{}
	}
	if _, exists := c.seen[key]; exists {
		return
	}
	c.seen[key] = struct{}{}
	c.items = append(c.items, append(json.RawMessage(nil), trimmed...))
}

func (c *codexWebsocketOutputCollector) result() []json.RawMessage {
	return cloneCodexWebsocketRawItems(c.items)
}
