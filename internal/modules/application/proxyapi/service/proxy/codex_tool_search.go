package proxy

import "fmt"

// CP-REQ-034: tool search is independent of the Responses Lite profile.
// The proxy forwards discovery configuration; it never executes client tools.
func validateCodexToolSearch(tool map[string]any) error {
	if execution, exists := tool["execution"]; exists && execution != "client" && execution != "server" {
		return fmt.Errorf("tool_search execution must be client or server")
	}
	if description := tool["description"]; description != nil {
		if _, ok := description.(string); !ok {
			return fmt.Errorf("tool_search description must be a string or null")
		}
	}
	if parameters := tool["parameters"]; parameters != nil {
		if _, ok := parameters.(map[string]any); !ok {
			return fmt.Errorf("tool_search parameters must be an object or null")
		}
	}
	return nil
}
