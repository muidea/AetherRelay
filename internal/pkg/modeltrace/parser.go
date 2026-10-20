package modeltrace

import (
	"encoding/json"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

type ProbeError struct {
	Code       string `json:"code"`
	Format     string `json:"format"`
	TextLength *int   `json:"textLength"`
	Expected   int    `json:"expectedCount"`
	Received   *int   `json:"receivedCount,omitempty"`
}

func (e *ProbeError) Error() string { return "MODELTRACE_PROBE_OUTPUT: " + e.Code }
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == '\uFEFF' || r == '\u0009' || r == '\u000B' || r == '\u000C' || r == '\u0020' || r == '\u00A0' || r == '\u000A' || r == '\u000D' || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Zs, r)
	})
}

var fence = regexp.MustCompile("(?is)^(`{3,}|~{3,})[ \\t]*(?:json)?[ \\t]*\\r?\\n([\\s\\S]*?)\\r?\\n[ \\t]*(`{3,}|~{3,})$")
var literal = regexp.MustCompile(`^\[[\x20\t\r\n]*[0-9]+(?:[\x20\t\r\n]*,[\x20\t\r\n]*[0-9]+)*[\x20\t\r\n]*\]$`)

// ValidateOutput preserves missing-text diagnostics for nullable collector output.
func ValidateOutput(text *string, expected int) ([]int, error) {
	if text == nil {
		return nil, &ProbeError{Code: "missing_text", Format: "none", Expected: expected}
	}
	return ValidateNumbers(*text, expected)
}

func ValidateNumbers(text string, expected int) ([]int, error) {
	textLength := len(utf16.Encode([]rune(text)))
	e := &ProbeError{Format: "none", TextLength: &textLength, Expected: expected}
	fail := func(code string) ([]int, error) { e.Code = code; return nil, e }
	if textLength > 5000 {
		return fail("output_too_long")
	}
	body := jsTrim(text)
	if body == "" {
		return fail("empty_answer")
	}
	e.Format = "bare"
	if strings.HasPrefix(body, "```") || strings.HasPrefix(body, "~~~") {
		e.Format = "code_fence"
		m := fence.FindStringSubmatch(body)
		if m == nil || m[1] != m[3] {
			return fail("invalid_code_fence")
		}
		body = jsTrim(m[2])
	}
	var raw json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(body))
	if decoder.Decode(&raw) != nil {
		return fail("invalid_json")
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return fail("invalid_json")
	}
	if len(raw) == 0 || raw[0] != '[' {
		return fail("not_array")
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return fail("invalid_json")
	}
	n := len(arr)
	e.Received = &n
	if n > 0 && !literal.MatchString(body) {
		return fail("non_literal_integer")
	}
	out := make([]int, n)
	for i, v := range arr {
		x, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.Trunc(x) != x || x < 1 || x > 355 {
			return fail("out_of_range")
		}
		out[i] = int(x)
	}
	if n < max(80, int(math.Ceil(float64(expected)*.55))) || n > int(math.Ceil(float64(expected)*1.25)) {
		return fail("count_out_of_bounds")
	}
	return out, nil
}

var digits = regexp.MustCompile(`[0-9]+`)

// ParseNumbers matches the core's permissive run extractor. Bundle input first uses ValidateNumbers.
func ParseNumbers(text string) []int {
	best := []int{}
	current := []int{}
	end := 0
	for _, m := range digits.FindAllStringIndex(text, -1) {
		letter := false
		for _, r := range text[end:m[0]] {
			if unicode.IsLetter(r) {
				letter = true
				break
			}
		}
		if len(current) > 0 && letter {
			if len(current) > len(best) {
				best = current
			}
			current = []int{}
		}
		v, err := strconv.Atoi(text[m[0]:m[1]])
		if err == nil && v >= 1 && v <= 355 {
			current = append(current, v)
		}
		end = m[1]
	}
	if len(current) > len(best) {
		best = current
	}
	return best
}
