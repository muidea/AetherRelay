package biz

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Preserve finalized hosted searches when a terminal response contains only
// the answer. Terminal items remain authoritative when the same ID is present.
func responseWithWebSearchItems(response []byte, items map[int]json.RawMessage) []byte {
	var object map[string]json.RawMessage
	if len(items) == 0 || json.Unmarshal(response, &object) != nil || object == nil {
		return response
	}
	var output []json.RawMessage
	if raw := object["output"]; len(raw) > 0 && json.Unmarshal(raw, &output) != nil {
		return response
	}
	indices := make([]int, 0, len(items))
	for index := range items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	changed := false
	for _, index := range indices {
		raw := items[index]
		var search struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(raw, &search) != nil || search.Type != "web_search_call" {
			continue
		}
		found := false
		for _, existing := range output {
			var item struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(existing, &item)
			if search.ID != "" && search.ID == item.ID || bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(existing)) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		position := min(max(index, 0), len(output))
		output = append(output, nil)
		copy(output[position+1:], output[position:])
		output[position] = raw
		changed = true
	}
	if !changed {
		return response
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return response
	}
	object["output"] = encoded
	encoded, err = json.Marshal(object)
	if err != nil {
		return response
	}
	return encoded
}
