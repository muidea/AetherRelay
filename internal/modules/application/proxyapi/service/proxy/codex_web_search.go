package proxy

import (
	"fmt"
	"strings"
)

// CP-REQ-033: validate known options without rewriting hosted search semantics.
// In particular, false must not be dropped or converted to a preview tool (which
// would ignore the cache-only restriction). Upstream owns extension support.
func validateCodexWebSearchTool(tool map[string]any) error {
	if value, exists := tool["external_web_access"]; exists {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("web_search external_web_access must be a boolean")
		}
	}
	if value, exists := tool["search_context_size"]; exists {
		if value != "low" && value != "medium" && value != "high" {
			return fmt.Errorf("web_search search_context_size must be low, medium, or high")
		}
	}
	if value := tool["user_location"]; value != nil {
		location, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("web_search user_location must be an approximate location object")
		}
		if typ, exists := location["type"]; exists && typ != "approximate" {
			return fmt.Errorf("web_search user_location.type must be approximate")
		}
		for _, field := range []string{"country", "region", "city", "timezone"} {
			if value := location[field]; value != nil {
				if _, ok := value.(string); !ok {
					return fmt.Errorf("web_search user_location.%s must be a string or null", field)
				}
			}
		}
	}
	if value := tool["filters"]; value != nil {
		filters, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("web_search filters must be an object")
		}
		if value, exists := filters["allowed_domains"]; exists {
			domains, ok := value.([]any)
			if !ok {
				return fmt.Errorf("web_search filters.allowed_domains must be an array of strings")
			}
			for _, value := range domains {
				domain, ok := value.(string)
				if !ok || strings.TrimSpace(domain) == "" {
					return fmt.Errorf("web_search filters.allowed_domains must contain non-empty strings")
				}
			}
		}
	}
	return nil
}

// Existing function/custom choice handling is unchanged. Only hosted search
// references need an accompanying top-level declaration; choices are not tools.
func validateCodexWebSearchChoice(value, tools any) error {
	choice, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if choice["type"] == "allowed_tools" {
		choices, _ := choice["tools"].([]any)
		for _, item := range choices {
			if err := validateCodexWebSearchChoice(item, tools); err != nil {
				return err
			}
		}
	}
	if choice["type"] != "web_search" {
		return nil
	}
	list, _ := tools.([]any)
	for _, raw := range list {
		if tool, ok := raw.(map[string]any); ok && tool["type"] == "web_search" {
			return nil
		}
	}
	return fmt.Errorf("web_search tool_choice requires a top-level web_search tool")
}
