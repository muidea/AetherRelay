// Package generation measures observed output generation, excluding queueing and TTFT.
package generation

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"aetherrelay/internal/pkg/aetherrelaycodex"
)

type Sample struct {
	FirstOutputAt time.Time
	Duration      time.Duration
	Partial       bool
	Buffered      bool // Explicit upstream buffering signal; false does not prove no buffering.
}

type Clock struct {
	mu         sync.Mutex
	first, end time.Time
	partial    bool
}

func (c *Clock) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.first = time.Time{}
	c.end = time.Time{}
	c.partial = false
}
func (c *Clock) Output(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.first.IsZero() && c.end.IsZero() {
		c.first = at
	}
}
func (c *Clock) Finish(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.end.IsZero() {
		c.end = at
	}
}
func (c *Clock) Snapshot(at time.Time) Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.first.IsZero() {
		return Sample{}
	}
	end := c.end
	partial := end.IsZero()
	if partial {
		end = at
	}
	duration := end.Sub(c.first)
	if duration <= 0 {
		return Sample{}
	}
	return Sample{FirstOutputAt: c.first, Duration: duration, Partial: partial || c.partial}
}

// ObserveSSE ignores metadata, role-only events and heartbeats. All documents in
// one wire line share a timestamp, including concatenated Responses documents.
func (c *Clock) ObserveSSE(line []byte, at time.Time) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(data, []byte("[DONE]")) {
		c.Finish(at)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return
		}
		output, terminal := classify(raw)
		if output {
			c.Output(at)
		}
		if terminal {
			var kind struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &kind)
			if kind.Type == "response.failed" || kind.Type == "response.incomplete" || kind.Type == "error" {
				c.Stop(at)
			} else {
				c.Finish(at)
			}
		}
	}
}
func classify(raw []byte) (bool, bool) {
	var v struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			JSON     string `json:"partial_json"`
		} `json:"delta"`
		Block struct {
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content_block"`
		Choices []struct {
			Text  string `json:"text"`
			Delta struct {
				Content       json.RawMessage `json:"content"`
				Reasoning     string          `json:"reasoning_content"`
				ReasoningText string          `json:"reasoning"`
				Tools         []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function_call"`
			} `json:"delta"`
		} `json:"choices"`
	}
	// Responses delta is a string, so parse its semantic output independently.
	var kind struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &kind) != nil {
		return false, false
	}
	if strings.HasPrefix(kind.Type, "response.") || kind.Type == "error" {
		terminal := kind.Type == "response.completed" || kind.Type == "response.incomplete" || kind.Type == "response.failed" || kind.Type == "error"
		return aetherrelaycodex.MeaningfulOutputEvent(raw), terminal
	}
	if json.Unmarshal(raw, &v) != nil {
		return false, false
	}
	if v.Type == "message_stop" {
		return false, true
	}
	if v.Type == "content_block_delta" {
		return v.Delta.Text != "" || v.Delta.Thinking != "" || v.Delta.JSON != "", false
	}
	if v.Type == "content_block_start" {
		return v.Block.Text != "" || v.Block.Thinking != "", false
	}
	for _, choice := range v.Choices {
		var text string
		_ = json.Unmarshal(choice.Delta.Content, &text)
		if choice.Text != "" || text != "" || choice.Delta.Reasoning != "" || choice.Delta.ReasoningText != "" || choice.Delta.Function.Arguments != "" {
			return true, false
		}
		for _, tool := range choice.Delta.Tools {
			if tool.Function.Arguments != "" {
				return true, false
			}
		}
	}
	return false, false
}

// Stop freezes a truncated generation at the observed read/cancellation failure.
func (c *Clock) Stop(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.end.IsZero() {
		c.end = at
		c.partial = true
	}
}
