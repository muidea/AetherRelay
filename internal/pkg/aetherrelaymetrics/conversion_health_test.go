package metrics

import (
	"reflect"
	"testing"
	"time"
)

func TestConversionIsNeutralToProviderHealthAtEveryHTTPStatus(t *testing.T) {
	r := NewRegistry()
	record := func(status int, outcome string) {
		r.RecordRequestPlan("codexoauth", "model", "messages", status, time.Millisecond, outcome, "", "", "", "anthropic_to_codex")
	}
	for range 3 {
		for _, status := range []int{200, 400, 502, 503} {
			record(status, "conversion")
		}
	}
	if len(r.ProviderHealthSnapshot()) != 0 {
		t.Fatal("conversion opened provider circuit")
	}
	for range 3 {
		record(502, "upstream_failed")
	}
	before := r.ProviderHealthSnapshot()["codexoauth"]
	if before.CircuitState != "open" {
		t.Fatal("genuine upstream failure did not open circuit")
	}
	for _, status := range []int{200, 400, 502, 503} {
		record(status, "conversion")
	}
	after := r.ProviderHealthSnapshot()["codexoauth"]
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("conversion changed existing health: before=%+v after=%+v", before, after)
	}
	record(200, "success")
	if r.ProviderHealthSnapshot()["codexoauth"].CircuitState != "closed" {
		t.Fatal("genuine success could not recover circuit")
	}
}
