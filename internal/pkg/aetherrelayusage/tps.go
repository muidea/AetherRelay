package usage

import (
	"math"
	"strconv"
	"time"
)

// GenerationSample carries independent E2E and observed output delivery rates.
// E2E can be available even when no output-stage timing was observed.
type GenerationSample struct {
	TPS                  *float64   `json:"tps"`
	OutputTokensKnown    bool       `json:"output_tokens_known"`
	FirstOutputAt        *time.Time `json:"first_output_at,omitempty"`
	GenerationDurationNS int64      `json:"-"`
	GenerationDurationMS float64    `json:"generation_duration_ms"`
	ObservedTPS          *float64   `json:"observed_tps"`
	GenerationBuffered   bool       `json:"generation_buffered"`
	GenerationPartial    bool       `json:"generation_partial"`
}

// TPSStats uses exactly the same eligible sample set for both numerator and
// denominator for each metric. Neither describes concurrent system throughput.
// Observed delivery retains explicit buffering without rewriting timings.
type TPSStats struct {
	TPS                             *float64 `json:"tps"`
	TPSSamples                      int64    `json:"tps_samples"`
	TPSEstimatedSamples             int64    `json:"tps_estimated_samples"`
	TPSPartialSamples               int64    `json:"tps_partial_samples"`
	TPSOutputTokens                 int64    `json:"tps_output_tokens"`
	TPSDurationMS                   int64    `json:"tps_duration_ms"`
	ObservedTPS                     *float64 `json:"observed_tps"`
	ObservedTPSSamples              int64    `json:"observed_tps_samples"`
	ObservedTPSEstimatedSamples     int64    `json:"observed_tps_estimated_samples"`
	ObservedTPSBufferedSamples      int64    `json:"observed_tps_buffered_samples"`
	ObservedTPSPartialSamples       int64    `json:"observed_tps_partial_samples"`
	ObservedTPSOutputTokens         int64    `json:"observed_tps_output_tokens"`
	ObservedTPSGenerationDurationMS float64  `json:"observed_tps_generation_duration_ms"`
}

func validGeneration(r CompleteRecord) bool {
	return r.OutputTokensKnown && !r.FirstOutputAt.IsZero() && r.GenerationDuration > 0
}
func generationSample(r CompleteRecord) GenerationSample {
	if !validGeneration(r) {
		return GenerationSample{}
	}
	at := r.FirstOutputAt.UTC()
	return makeGenerationSample(at, int64(r.GenerationDuration), r.OutputTokens, r.GenerationPartial || r.Outcome != "success", r.GenerationBuffered)
}
func makeGenerationSample(at time.Time, ns, tokens int64, partial, buffered bool) GenerationSample {
	if ns <= 0 || at.IsZero() || tokens < 0 {
		return GenerationSample{}
	}
	tps := outputRate(tokens, float64(ns)/float64(time.Second), true)
	return GenerationSample{FirstOutputAt: &at, GenerationDurationNS: ns, GenerationDurationMS: float64(ns) / float64(time.Millisecond), ObservedTPS: tps, GenerationPartial: partial, GenerationBuffered: buffered}
}
func (s *TPSStats) finish() {
	s.TPS = outputRate(s.TPSOutputTokens, float64(s.TPSDurationMS)/1000, s.TPSSamples > 0)
	if s.ObservedTPSSamples > 0 && s.ObservedTPSGenerationDurationMS > 0 {
		s.ObservedTPS = outputRate(s.ObservedTPSOutputTokens, s.ObservedTPSGenerationDurationMS/1000, true)
	}
}
func (s *TPSStats) add(e Event) {
	if e.TPS != nil && e.State == StateCompleted {
		s.TPSSamples++
		s.TPSOutputTokens += e.OutputTokens
		s.TPSDurationMS += e.DurationMS
		if e.Estimated {
			s.TPSEstimatedSamples++
		}
		if e.GenerationPartial || e.Outcome != "success" {
			s.TPSPartialSamples++
		}
	}
	s.finish()
	if e.State != StateCompleted || e.ObservedTPS == nil || e.GenerationDurationNS <= 0 {
		return
	}
	s.ObservedTPSSamples++
	if e.GenerationBuffered {
		s.ObservedTPSBufferedSamples++
	}
	s.ObservedTPSOutputTokens += e.OutputTokens
	s.ObservedTPSGenerationDurationMS += e.GenerationDurationMS
	if e.Estimated {
		s.ObservedTPSEstimatedSamples++
	}
	if e.GenerationPartial {
		s.ObservedTPSPartialSamples++
	}
	s.finish()
}

// Keep filter/cursor columns unambiguous and preserve all ordinary event totals.
const tpsEvents = `(SELECT e.*, g.first_output_at, g.generation_duration_ns, g.partial AS generation_partial, b.event_id IS NOT NULL AS generation_buffered, coalesce(o.known, e.output_tokens > 0 OR g.event_id IS NOT NULL) AS output_tokens_known FROM usage_events e LEFT JOIN usage_generation g ON e.event_id=g.event_id LEFT JOIN usage_generation_buffering b ON e.event_id=b.event_id LEFT JOIN usage_output_observation o ON e.event_id=o.event_id) AS usage_events`
const e2eEligible = "state = 'completed' AND output_tokens_known AND output_tokens >= 0 AND duration_ms > 0"
const tpsAggregates = `,
 count(*) FILTER (WHERE ` + e2eEligible + `),
 count(*) FILTER (WHERE ` + e2eEligible + ` AND estimated),
 count(*) FILTER (WHERE ` + e2eEligible + ` AND (generation_partial OR outcome IS DISTINCT FROM 'success')),
 coalesce(sum(output_tokens) FILTER (WHERE ` + e2eEligible + `), 0),
 coalesce(sum(duration_ms) FILTER (WHERE ` + e2eEligible + `), 0),
 count(generation_duration_ns),
 count(generation_duration_ns) FILTER (WHERE estimated),
 count(generation_duration_ns) FILTER (WHERE generation_partial),
 count(generation_duration_ns) FILTER (WHERE generation_buffered),
 coalesce(sum(output_tokens) FILTER (WHERE generation_duration_ns IS NOT NULL), 0),
 coalesce(sum(generation_duration_ns)::DOUBLE / 1000000, 0)`

// outputRate is shared by events, aggregates and CSV. A known zero is distinct
// from missing usage. Durations use the same millisecond precision as the API.
func outputRate(tokens int64, seconds float64, known bool) *float64 {
	if !known || tokens < 0 || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return nil
	}
	value := float64(tokens) / seconds
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}
func setE2ETPS(e *Event) {
	e.TPS = outputRate(e.OutputTokens, float64(e.DurationMS)/1000, e.State == StateCompleted && e.OutputTokensKnown)
}
func rateCSV(rate *float64) string {
	if rate == nil {
		return ""
	}
	return strconv.FormatFloat(*rate, 'f', -1, 64)
}
func tpsCSV(s GenerationSample) []string {
	row := []string{"", "", rateCSV(s.TPS), "", "", rateCSV(s.ObservedTPS), strconv.FormatBool(s.OutputTokensKnown)}
	if s.ObservedTPS != nil && s.FirstOutputAt != nil {
		row[0] = s.FirstOutputAt.Format(time.RFC3339Nano)
		row[1] = strconv.FormatFloat(s.GenerationDurationMS, 'f', -1, 64)
		row[3], row[4] = strconv.FormatBool(s.GenerationPartial), strconv.FormatBool(s.GenerationBuffered)
	}
	return row
}

func cloneGenerationSample(s GenerationSample) GenerationSample {
	if s.TPS != nil {
		value := *s.TPS
		s.TPS = &value
	}
	if s.ObservedTPS != nil {
		value := *s.ObservedTPS
		s.ObservedTPS = &value
	}
	if s.FirstOutputAt != nil {
		value := *s.FirstOutputAt
		s.FirstOutputAt = &value
	}
	return s
}
