package metrics

import "testing"

// CP-OBS-017: unknown/failed cache observations must not become misses or rate inputs.
func TestCacheObservationsRequireKnownSuccessfulUsage(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < 10; i++ {
		r.RecordTokensObserved("chatgptweb", "gpt-6", 100, 20, 0, 0, false, true)
		r.RecordTokensObserved("chatgptweb", "gpt-6", 100, 0, 0, 0, false, false)
		r.RecordTokensObserved("chatgptweb", "gpt-6", 100, 0, 0, 0, true, false)
	}
	e := NewSLOEvaluator(r, SLOConfig{CacheHitRateMin: 0.2}, "", nil)
	if violations := e.evaluate(r.snapshotForSLO()); len(violations) != 0 {
		t.Fatalf("unknown cache emitted SLO violation: %+v", violations)
	}
	key := tokenKey{Provider: "chatgptweb", Model: "gpt-6"}
	if r.inputTokens[key] != 3000 || r.outputTokens[key] != 200 {
		t.Fatal("ordinary token accounting was dropped")
	}
	if _, ok := computeCacheHitRateLocked(r)[key]; ok {
		t.Fatal("unknown cache rate exposed as zero")
	}
	for i := 0; i < 10; i++ {
		r.RecordTokensObserved("chatgptweb", "gpt-6", 100, 20, 0, 0, true, true)
	}
	if violations := e.evaluate(r.snapshotForSLO()); len(violations) != 1 || violations[0].Rule != "cache_hit_rate_min" {
		t.Fatalf("known misses lost: %+v", violations)
	}
	r.RecordTokensObserved("chatgptweb", "gpt-6", 100, 20, 100, 0, true, true)
	if got := computeCacheHitRateLocked(r)[key]; got != float64(100)/1100 {
		t.Fatalf("rate diluted by unknown/failed usage: %v", got)
	}
	if r.cacheHits[key] != 1 || r.cacheMisses[key] != 10 {
		t.Fatalf("hits=%d misses=%d", r.cacheHits[key], r.cacheMisses[key])
	}
}
