package proxy

import "fmt"

// 转换结构预算。三项互相独立:顶层项数、协议内容块、system 数组块各自计量。
// 内容块预算按线上长会话实测放宽(2026-09-22 rounds 253/276,累计 259),
// 其余预算不随之放宽。
const (
	maxConversionMessages      = 256
	maxConversionContentBlocks = 512
	maxConversionSystemBlocks  = 256
	maxConversionTreeNodes     = 65536
)

// Paths and kinds originate exclusively in adapter code, never in business
// object keys. Errors may safely be included in logs and client responses.
type conversionLimitError struct {
	Path, Kind    string
	Limit, Actual int
}

func (e *conversionLimitError) Error() string {
	return fmt.Sprintf("%s exceeds conversion %s limit (actual=%d, limit=%d)", e.Path, e.Kind, e.Actual, e.Limit)
}

// Count protocol envelopes separately from arbitrary tool input JSON. Keep a
// separate node/depth budget for the entire tree, including business arrays.
func validateConversionTree(value any, label string) error {
	items, _ := value.([]any)
	if len(items) > maxConversionMessages {
		return &conversionLimitError{label, "messages", maxConversionMessages, len(items)}
	}
	nodes := 0
	var walk func(any, int) error
	walk = func(node any, depth int) error {
		if depth > maxConversionSchemaDepth {
			return &conversionLimitError{label, "depth", maxConversionSchemaDepth, depth}
		}
		nodes++
		if nodes > maxConversionTreeNodes {
			return &conversionLimitError{label, "tree_nodes", maxConversionTreeNodes, nodes}
		}
		switch v := node.(type) {
		case map[string]any:
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value, 0); err != nil {
		return err
	}
	blocks := 0
	var content func(any, string) error
	content = func(raw any, path string) error {
		parts, ok := raw.([]any)
		if !ok {
			return nil
		}
		blocks += len(parts)
		if blocks > maxConversionContentBlocks {
			return &conversionLimitError{path, "content_blocks", maxConversionContentBlocks, blocks}
		}
		for i, raw := range parts {
			block, _ := raw.(map[string]any)
			if label == "messages" && block["type"] == "tool_result" {
				if err := content(block["content"], fmt.Sprintf("%s[%d].content", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for i, raw := range items {
		item, _ := raw.(map[string]any)
		// Responses function arguments/output are business data, not content.
		if label == "input" && item["type"] != nil && item["type"] != "message" {
			continue
		}
		if err := content(item["content"], fmt.Sprintf("%s[%d].content", label, i)); err != nil {
			return err
		}
	}
	return nil
}
