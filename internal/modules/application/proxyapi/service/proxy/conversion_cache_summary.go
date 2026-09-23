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
		CacheKey     json.RawMessage `json:"prompt_cache_key"`
		Input        []struct {
			Role string `json:"role"`
		} `json:"input"`
	}
	if json.Unmarshal(encoded, &body) != nil {
		return nil
	}
	digest := func(raw json.RawMessage) string {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	systems := 0
	for _, item := range body.Input {
		if item.Role == "system" {
			systems++
		}
	}
	return []slog.Attr{
		slog.String("instructions_digest", digest(body.Instructions)),
		slog.String("tools_digest", digest(body.Tools)),
		slog.String("prompt_cache_key_digest", digest(body.CacheKey)),
		slog.Int("input_items", len(body.Input)),
		slog.Int("history_system_messages", systems),
	}
}
