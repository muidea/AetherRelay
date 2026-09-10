package aetherrelaycodex

import (
	"encoding/json"
	"math/big"
)

const complexUnionBranchThreshold = 8

var schemaMapKeywords = [...]string{
	"properties", "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies",
}

var schemaValueKeywords = [...]string{
	"items", "prefixItems", "contains", "additionalProperties", "propertyNames", "unevaluatedProperties",
	"unevaluatedItems", "additionalItems", "contentSchema", "anyOf", "oneOf", "allOf", "not", "if", "then", "else",
}

// NormalizeToolSchemas mutates the decoded request with the semantic-preserving
// compatibility rules in CP-REQ-035.
func NormalizeToolSchemas(value any) {
	tools, ok := value.([]any)
	if !ok {
		return
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		normalizeTool(tool)
	}
}

func normalizeTool(tool map[string]any) {
	typ, _ := tool["type"].(string)
	if typ == "namespace" {
		NormalizeToolSchemas(tool["tools"])
		return
	}
	if typ == "function" || typ == "custom" {
		if parameters, exists := tool["parameters"]; exists {
			tool["parameters"] = normalizedParameters(parameters)
		}
	}
	if function, ok := tool["function"].(map[string]any); ok {
		if parameters, exists := function["parameters"]; exists {
			function["parameters"] = normalizedParameters(parameters)
		}
	}
	NormalizeToolSchemas(tool["tools"])
}

func normalizedParameters(value any) map[string]any {
	root, ok := value.(map[string]any)
	if !ok || root == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	normalizeSchema(root, true, 0)
	return root
}

func normalizeSchema(schema map[string]any, parameterRoot bool, depth int) {
	if schema == nil || depth > 128 {
		return
	}
	delete(schema, "$schema")
	delete(schema, "$id")
	if pattern, ok := schema["pattern"].(string); ok && hasUnsupportedUnicodePropertyEscape(pattern) {
		delete(schema, "pattern")
	}
	if patternProperties, ok := schema["patternProperties"].(map[string]any); ok {
		for pattern, child := range patternProperties {
			if hasUnsupportedUnicodePropertyEscape(pattern) {
				delete(patternProperties, pattern)
				continue
			}
			normalizeSchemaValue(child, depth+1)
		}
	}
	if parameterRoot {
		if typ, exists := schema["type"]; !exists || typ == nil || typ == "" {
			schema["type"] = "object"
		}
	}
	if schemaIncludesObject(schema["type"]) {
		if properties, ok := schema["properties"]; !ok || properties == nil {
			schema["properties"] = map[string]any{}
		}
	}
	normalizePureConstUnion(schema)
	for _, key := range schemaMapKeywords {
		if key == "patternProperties" {
			continue
		}
		if children, ok := schema[key].(map[string]any); ok {
			for _, child := range children {
				normalizeSchemaValue(child, depth+1)
			}
		}
	}
	for _, key := range schemaValueKeywords {
		normalizeSchemaValue(schema[key], depth+1)
	}
}

func normalizeSchemaValue(value any, depth int) {
	switch typed := value.(type) {
	case map[string]any:
		normalizeSchema(typed, false, depth)
	case []any:
		for _, item := range typed {
			normalizeSchemaValue(item, depth+1)
		}
	}
}

func schemaIncludesObject(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed == "object"
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text == "object" {
				return true
			}
		}
	}
	return false
}

func normalizePureConstUnion(schema map[string]any) {
	_, one := schema["oneOf"]
	_, anyOf := schema["anyOf"]
	if one && anyOf {
		return
	}
	key := "oneOf"
	if !one {
		key = "anyOf"
	}
	branches, ok := schema[key].([]any)
	if !ok || len(branches) < complexUnionBranchThreshold {
		return
	}
	values := make([]any, 0, len(branches))
	keys := make([]string, 0, len(branches))
	seen := make(map[string]struct{}, len(branches))
	for _, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			return
		}
		for branchKey := range branch {
			if branchKey != "const" && branchKey != "description" && branchKey != "title" {
				return
			}
		}
		value, exists := branch["const"]
		if !exists {
			return
		}
		canonical, ok := canonicalScalar(value)
		if !ok {
			return
		}
		if _, duplicate := seen[canonical]; duplicate {
			return
		}
		seen[canonical] = struct{}{}
		keys = append(keys, canonical)
		values = append(values, value)
	}
	if existing, ok := schema["enum"].([]any); ok {
		if !sameScalarSet(existing, keys) {
			return
		}
		delete(schema, key)
		return
	}
	schema["enum"] = values
	delete(schema, key)
}

func sameScalarSet(values []any, expected []string) bool {
	if len(values) != len(expected) {
		return false
	}
	set := make(map[string]struct{}, len(expected))
	for _, key := range expected {
		set[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key, ok := canonicalScalar(value)
		if !ok {
			return false
		}
		if _, found := set[key]; !found {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return len(seen) == len(set)
}

func canonicalScalar(value any) (string, bool) {
	switch typed := value.(type) {
	case nil:
		return "null", true
	case string:
		return "s:" + typed, true
	case bool:
		if typed {
			return "b:true", true
		}
		return "b:false", true
	case json.Number:
		var rational big.Rat
		if _, ok := rational.SetString(string(typed)); ok {
			return "n:" + rational.RatString(), true
		}
		return "n:" + string(typed), true
	case float64:
		encoded, _ := json.Marshal(typed)
		return "n:" + string(encoded), true
	default:
		return "", false
	}
}

func hasUnsupportedUnicodePropertyEscape(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '\\' {
			continue
		}
		if i+2 < len(pattern) && (pattern[i+1] == 'p' || pattern[i+1] == 'P') && pattern[i+2] == '{' {
			return true
		}
		i++
	}
	return false
}
