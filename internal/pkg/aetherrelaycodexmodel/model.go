// Package codexmodel observes upstream-declared Codex model identities only.
package codexmodel

import (
	"bytes"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
)

type Observation struct {
	UpstreamRequestModel string `json:"upstream_request_model,omitempty"`
	ResponseModel        string `json:"response_model,omitempty"`
	ModelSubstituted     bool   `json:"model_substituted,omitempty"`
}

func New(requested, served string) Observation {
	requested, served = safeModel(requested), safeModel(served)
	return Observation{requested, served, IsSubstituted(requested, served)}
}

func safeModel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

func RequestModel(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	model := gjson.GetBytes(body, "model")
	if model.Type != gjson.String {
		return ""
	}
	return safeModel(model.String())
}

// Observer retains the latest valid declaration, finalized by a terminal model.
// An absent terminal model does not erase the last valid declaration.
type Observer struct {
	Model string
	final bool
}

func (o *Observer) Observe(payload []byte) {
	if o.final {
		return
	}
	trimmed := bytes.TrimSpace(payload)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		o.observeJSON(trimmed)
		return
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			o.observeJSON(bytes.TrimSpace(line[5:]))
		}
	}
}

func (o *Observer) observeJSON(line []byte) {
	if o.final || len(line) == 0 || line[0] != '{' {
		return
	}
	kind := gjson.GetBytes(line, "type")
	path, terminal := "response.model", false
	switch kind.String() {
	case "response.created", "response.in_progress":
	case "response.completed", "response.incomplete", "response.done":
		terminal = true
	default:
		if kind.Exists() || gjson.GetBytes(line, "object").String() != "response" {
			return
		}
		path, terminal = "model", true
	}
	if !gjson.ValidBytes(line) {
		return
	}
	value := gjson.GetBytes(line, path)
	if value.Type != gjson.String {
		return
	}
	if model := safeModel(value.String()); model != "" {
		o.Model = model
		if terminal {
			o.final = true
		}
	}
}

func normalize(model string) string {
	model = strings.ToLower(safeModel(model))
	// Match CLIProxyAPI's trailing reasoning suffix convention.
	if strings.HasSuffix(model, ")") {
		if at := strings.LastIndex(model, "("); at >= 0 {
			model = strings.TrimSpace(model[:at])
		}
	}
	return model
}

func IsSubstituted(requested, served string) bool {
	requested, served = normalize(requested), normalize(served)
	return requested != "" && served != "" && requested != served && !datedAlias(requested, served) && !datedAlias(served, requested)
}

func datedAlias(base, dated string) bool {
	if !strings.HasPrefix(dated, base+"-") {
		return false
	}
	suffix := strings.TrimPrefix(dated, base+"-")
	if len(suffix) == 10 && suffix[4] == '-' && suffix[7] == '-' {
		suffix = suffix[:4] + suffix[5:7] + suffix[8:]
	} else if len(suffix) != 8 {
		return false
	}
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
