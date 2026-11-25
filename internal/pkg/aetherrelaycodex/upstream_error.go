package aetherrelaycodex

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// UpstreamError is the bounded error already extracted from an upstream error
// object. It contains no raw response body or headers and never controls retry.
type UpstreamError struct {
	UpstreamErrorType    string `json:"upstream_error_type,omitempty"`
	UpstreamErrorCode    string `json:"upstream_error_code,omitempty"`
	UpstreamErrorParam   string `json:"upstream_error_param,omitempty"`
	UpstreamErrorMessage string `json:"upstream_error_message,omitempty"`
}

func (e UpstreamError) Empty() bool {
	return e.UpstreamErrorType == "" && e.UpstreamErrorCode == "" && e.UpstreamErrorParam == "" && e.UpstreamErrorMessage == ""
}

func (e UpstreamError) Bounded() UpstreamError {
	e.UpstreamErrorType = ErrorToken(e.UpstreamErrorType, 64)
	e.UpstreamErrorCode = ErrorToken(e.UpstreamErrorCode, 96)
	e.UpstreamErrorParam = ErrorToken(e.UpstreamErrorParam, 256)
	e.UpstreamErrorMessage = ErrorText(e.UpstreamErrorMessage, 512)
	return e
}

// ExtractUpstreamError extracts bounded structured fields from an upstream JSON
// error body across OpenAI, DeepSeek, Anthropic, and Codex envelopes. Non-JSON,
// HTML, and empty bodies return the zero value.
func ExtractUpstreamError(body []byte) UpstreamError {
	type rawErrorObj struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Param   json.RawMessage `json:"param"`
		Message string          `json:"message"`
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return UpstreamError{}
	}
	hasRawValue := func(raw json.RawMessage) bool {
		trimmed := bytes.TrimSpace(raw)
		return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
	}
	extractScalarToken := func(raw json.RawMessage) string {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return ""
		}
		var str string
		if json.Unmarshal(trimmed, &str) == nil {
			return str
		}
		var num json.Number
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if dec.Decode(&num) == nil {
			return num.String()
		}
		return ""
	}
	decodeNode := func(raw json.RawMessage) (rawErrorObj, bool) {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return rawErrorObj{}, false
		}
		var str string
		if json.Unmarshal(trimmed, &str) == nil && str != "" {
			return rawErrorObj{Message: str}, true
		}
		var obj rawErrorObj
		if json.Unmarshal(trimmed, &obj) == nil && (obj.Type != "" || hasRawValue(obj.Code) || obj.Message != "" || hasRawValue(obj.Param)) {
			return obj, true
		}
		return rawErrorObj{}, false
	}
	toBounded := func(obj rawErrorObj) UpstreamError {
		return UpstreamError{
			UpstreamErrorType:    obj.Type,
			UpstreamErrorCode:    extractScalarToken(obj.Code),
			UpstreamErrorParam:   extractScalarToken(obj.Param),
			UpstreamErrorMessage: obj.Message,
		}.Bounded()
	}

	if obj, ok := decodeNode(envelope["error"]); ok {
		return toBounded(obj)
	}
	for _, wrapperKey := range []string{"response", "body"} {
		var wrapper struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(envelope[wrapperKey], &wrapper) == nil {
			if obj, ok := decodeNode(wrapper.Error); ok {
				return toBounded(obj)
			}
		}
	}
	var detail string
	if json.Unmarshal(envelope["detail"], &detail) == nil && detail != "" {
		return UpstreamError{UpstreamErrorMessage: detail}.Bounded()
	}
	if obj, ok := decodeNode(body); ok && (obj.Message != "" || (obj.Type != "" && hasRawValue(obj.Code))) {
		return toBounded(obj)
	}
	return UpstreamError{}
}

// ErrorToken and ErrorText retain the existing Codex error projection policy.
func ErrorToken(value string, limit int) string {
	value = boundedErrorString(strings.TrimSpace(value), limit)
	for _, char := range value {
		if !(char == '_' || char == '-' || char == '.' || char == '[' || char == ']' || char >= '0' && char <= '9' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z') {
			return ""
		}
	}
	return value
}

func ErrorText(value string, limit int) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, strings.TrimSpace(value))
	return strings.TrimSpace(boundedErrorString(value, limit))
}

func boundedErrorString(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
