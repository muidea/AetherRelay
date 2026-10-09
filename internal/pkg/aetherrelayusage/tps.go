package usage

import (
	"strconv"
	"time"
)

// GenerationSample is absent for legacy events and unobservable unary output.
type GenerationSample struct {
	FirstOutputAt        *time.Time `json:"first_output_at,omitempty"`
	GenerationDurationNS int64      `json:"-"`
	GenerationDurationMS float64    `json:"generation_duration_ms"`
	TPS                  *float64   `json:"tps"`
	GenerationPartial    bool       `json:"generation_partial"`
}

// TPSStats uses exactly the same eligible sample set for both numerator and
// denominator. It describes generation speed, not concurrent system throughput.
type TPSStats struct {
	TPS                     *float64 `json:"tps"`
	TPSSamples              int64    `json:"tps_samples"`
	TPSEstimatedSamples     int64    `json:"tps_estimated_samples"`
	TPSPartialSamples       int64    `json:"tps_partial_samples"`
	TPSOutputTokens         int64    `json:"tps_output_tokens"`
	TPSGenerationDurationMS float64  `json:"tps_generation_duration_ms"`
}

func validGeneration(r CompleteRecord) bool {
	return r.OutputTokensKnown && !r.FirstOutputAt.IsZero() && r.GenerationDuration > 0
}
func generationSample(r CompleteRecord) GenerationSample {
	if !validGeneration(r) {
		return GenerationSample{}
	}
	at := r.FirstOutputAt.UTC()
	return makeGenerationSample(at, int64(r.GenerationDuration), r.OutputTokens, r.GenerationPartial || r.Outcome != "success")
}
func makeGenerationSample(at time.Time, ns, tokens int64, partial bool) GenerationSample {
	if ns <= 0 || at.IsZero() {
		return GenerationSample{}
	}
	tps := float64(tokens) / (float64(ns) / float64(time.Second))
	return GenerationSample{FirstOutputAt: &at, GenerationDurationNS: ns, GenerationDurationMS: float64(ns) / float64(time.Millisecond), TPS: &tps, GenerationPartial: partial}
}
func (s *TPSStats) finish() {
	if s.TPSSamples > 0 && s.TPSGenerationDurationMS > 0 {
		value := float64(s.TPSOutputTokens) / (s.TPSGenerationDurationMS / 1000)
		s.TPS = &value
	}
}
func (s *TPSStats) add(e Event) {
	if e.State != StateCompleted || e.TPS == nil || e.GenerationDurationNS <= 0 {
		return
	}
	s.TPSSamples++
	s.TPSOutputTokens += e.OutputTokens
	s.TPSGenerationDurationMS += e.GenerationDurationMS
	if e.Estimated {
		s.TPSEstimatedSamples++
	}
	if e.GenerationPartial {
		s.TPSPartialSamples++
	}
	s.finish()
}

// Keep filter/cursor columns unambiguous and preserve all ordinary event totals.
const generationEvents = `(SELECT e.*, g.first_output_at, g.generation_duration_ns, g.partial AS generation_partial FROM usage_events e LEFT JOIN usage_generation g ON e.event_id=g.event_id) AS usage_events`
const generationAggregates = `,
 count(generation_duration_ns),
 count(generation_duration_ns) FILTER (WHERE estimated),
 count(generation_duration_ns) FILTER (WHERE generation_partial),
 coalesce(sum(output_tokens) FILTER (WHERE generation_duration_ns IS NOT NULL), 0),
 coalesce(sum(generation_duration_ns)::DOUBLE / 1000000, 0)`

func generationCSV(s GenerationSample) []string {
	if s.TPS == nil || s.FirstOutputAt == nil {
		return []string{"", "", "", ""}
	}
	return []string{s.FirstOutputAt.Format(time.RFC3339Nano), strconv.FormatFloat(s.GenerationDurationMS, 'f', -1, 64), strconv.FormatFloat(*s.TPS, 'f', -1, 64), strconv.FormatBool(s.GenerationPartial)}
}

func cloneGenerationSample(s GenerationSample) GenerationSample {
	if s.TPS != nil {
		value := *s.TPS
		s.TPS = &value
	}
	if s.FirstOutputAt != nil {
		value := *s.FirstOutputAt
		s.FirstOutputAt = &value
	}
	return s
}
