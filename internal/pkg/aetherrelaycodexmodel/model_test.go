package codexmodel

import (
	"strings"
	"testing"
	"time"
)

func TestModelSubstitutionRules(t *testing.T) {
	for _, tc := range []struct {
		request, response string
		want              bool
	}{
		{"gpt-6.1-sol", "gpt-6-luna", true},
		{" GPT-6.1-SOL(high) ", "gpt-6.1-sol", false},
		{"gpt-6.1-sol", "gpt-6.1-sol-2026-10-14", false},
		{"gpt-6.1-sol-20261014", "gpt-6.1-sol", false},
		{"gpt-6.1-sol-20261014", "gpt-6.1-sol-20261015", true},
		{"gpt-6.1-sol", "gpt-6.1-sol-preview", true},
		{"gpt-6.1-sol", "", false}, {"", "gpt-6-luna", false},
		{"gpt-6.1-sol", "private\nvalue", false},
	} {
		if got := IsSubstituted(tc.request, tc.response); got != tc.want {
			t.Fatalf("%q -> %q: %v", tc.request, tc.response, got)
		}
	}
}

func TestLatestValidDeclarationAndHeaderExclusion(t *testing.T) {
	var o Observer
	o.Observe([]byte(`data: {"type":"response.created","response":{"model":"gpt-6-luna"}}`))
	o.Observe([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-safety-buffering-faster-model":"gpt-6-luna"}}`))
	o.Observe([]byte(`data: {"type":"response.completed","response":{"model":"gpt-6.1-sol"}}`))
	o.Observe([]byte(`{"type":"response.created","response":{"model":"gpt-6-luna"}}`))
	if o.Model != "gpt-6.1-sol" || New("gpt-6.1-sol", o.Model).ModelSubstituted {
		t.Fatal(o)
	}
	var missing Observer
	missing.Observe([]byte(`{"type":"response.created","response":{"model":"gpt-6-luna"}}`))
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"I am gpt-6.1-sol","response":{"model":"gpt-6.1-sol"}}`,
		`{"type":"response.completed","response":{}}`,
		`{"type":"response.completed","response":{"model":123}}`,
		`{"type":"response.completed","response":{"model":"` + strings.Repeat("x", 129) + `"}}`,
	} {
		missing.Observe([]byte(payload))
	}
	if missing.Model != "gpt-6-luna" {
		t.Fatal(missing)
	}
	var jsonObject Observer
	jsonObject.Observe([]byte(`{
  "object": "response",
  "model": "gpt-6-luna",
  "output": []
}`))
	if jsonObject.Model != "gpt-6-luna" {
		t.Fatal(jsonObject)
	}
	var compact Observer
	compact.Observe([]byte(`{"object":"response.compaction","model":"gpt-6-luna"}`))
	if compact.Model != "" {
		t.Fatal(compact)
	}
}

func TestWarningWindowIsPerAccountAndModelPair(t *testing.T) {
	var l WarningLimiter
	at := time.Unix(0, 0)
	if !l.Allow("slot-a", "GPT-6.1-SOL(high)", "gpt-6-luna", at) || l.Allow("slot-a", "gpt-6.1-sol", "gpt-6-luna", at.Add(time.Minute)) {
		t.Fatal("repeat not throttled")
	}
	if !l.Allow("slot-b", "gpt-6.1-sol", "gpt-6-luna", at) || !l.Allow("slot-a", "gpt-6.1-sol", "gpt-6-astra", at) {
		t.Fatal("different account/pair suppressed")
	}
	if !l.Allow("slot-a", "gpt-6.1-sol", "gpt-6-luna", at.Add(10*time.Minute)) {
		t.Fatal("window did not reopen")
	}
}
