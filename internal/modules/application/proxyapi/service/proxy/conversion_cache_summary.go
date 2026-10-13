package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
)

// Diagnostic only: never use these digests to route accounts or change cache
// keys. No prompt, tool schema, session ID or raw cache key is logged.
func codexConversionCacheSummary(encoded []byte) []slog.Attr {
	var body struct {
		Instructions json.RawMessage `json:"instructions"`
		Tools        json.RawMessage `json:"tools"`
		Input        []struct {
			Role string `json:"role"`
			Type string `json:"type"`
		} `json:"input"`
	}
	if json.Unmarshal(encoded, &body) != nil {
		return nil
	}
	digest := func(raw json.RawMessage) string {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	instructions, calls, results := 0, 0, 0
	for _, item := range body.Input {
		if item.Role == "system" || item.Role == "developer" {
			instructions++
		}
		switch item.Type {
		case "function_call":
			calls++
		case "function_call_output":
			results++
		}
	}
	return []slog.Attr{
		slog.String("instructions_digest", digest(body.Instructions)),
		slog.String("tools_digest", digest(body.Tools)),
		slog.Int("input_items", len(body.Input)),
		slog.Int("history_instruction_messages", instructions),
		slog.Int("tool_calls", calls),
		slog.Int("tool_results", results),
	}
}
