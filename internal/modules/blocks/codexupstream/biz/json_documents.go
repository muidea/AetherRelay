package biz

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

const (
	maxCodexConcatenatedJSONDocuments = 16
	maxCodexConcatenatedJSONBytes     = 16 << 20
)

func splitCodexJSONDocuments(payload []byte) ([][]byte, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || len(payload) > maxCodexConcatenatedJSONBytes || json.Valid(payload) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	documents := make([][]byte, 0, 2)
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err != nil {
			return documents, err == io.EOF && len(documents) > 1
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &envelope) != nil || strings.TrimSpace(envelope.Type) == "" || strings.ContainsAny(envelope.Type, "\r\n") || len(documents) == maxCodexConcatenatedJSONDocuments {
			return nil, false
		}
		documents = append(documents, bytes.TrimSpace(raw))
	}
}

func expandCodexSSELine(line []byte) [][]byte {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return [][]byte{line}
	}
	documents, repaired := splitCodexJSONDocuments([]byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))))
	if !repaired {
		return [][]byte{line}
	}
	result := make([][]byte, 0, len(documents))
	for _, document := range documents {
		result = append(result, append(append([]byte("data: "), document...), '\n', '\n'))
	}
	return result
}
