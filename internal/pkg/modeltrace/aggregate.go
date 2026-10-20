package modeltrace

import (
	"context"
	"math"
	"sort"
	"strconv"
)

// analyze preserves the core's permissive parsing for oracle comparison. Evidence bundles are strictly validated before calling it.
func analyze(ctx context.Context, outputs []Output, b *bank) (*Analysis, error) {

	a := &Analysis{Method: "统一全局稳健数字指纹", Diagnostics: []Diagnostic{}}
	sum := make([]float64, len(b.Models))
	pooled := make([]float64, 355)
	for i, o := range outputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nums := ParseNumbers(o.Text)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		minimum := 80
		if o.ExpectedCount != 0 {
			minimum = max(80, int(math.Ceil(float64(o.ExpectedCount)*.55)))
		}
		accepted := len(nums) >= minimum
		a.Diagnostics = append(a.Diagnostics, Diagnostic{i, len(nums), minimum, accepted})
		if !accepted {
			continue
		}
		a.Used++
		s := scoreNumbers(nums, b)
		for j, v := range s {
			sum[j] += v
		}
		for j, v := range counts(nums) {
			pooled[j] += v
		}
	}
	if a.Used == 0 {
		return nil, ErrNoUsableOutputs
	}
	key := strconv.Itoa(min(a.Used, 3))
	cal := b.Calibration[key]
	a.Calibration.Queries = key
	a.Calibration.Beta = cal.Beta
	a.Calibration.Accuracy = cal.Accuracy
	logits := make([]float64, len(sum))
	maximum := math.Inf(-1)
	for i := range sum {
		sum[i] /= float64(a.Used)
		logits[i] = sum[i] * cal.Beta
		maximum = math.Max(maximum, logits[i])
	}
	total := 0.
	for i, v := range logits {
		logits[i] = math.Exp(v - maximum)
		total += logits[i]
	}
	familyIdx := map[string]int{}
	for _, m := range b.Models {
		family := m.Family
		if family == "" {
			family = "models"
		}
		if _, ok := familyIdx[family]; !ok {
			name := m.FamilyName
			if name == "" {
				name = family
			}
			familyIdx[family] = len(a.Families)
			a.Families = append(a.Families, Family{Family: family, DisplayName: name})
		}
	}
	for i, m := range b.Models {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		family := m.Family
		if family == "" {
			family = "models"
		}
		a.Results = append(a.Results, Candidate{Model: m.ID, DisplayName: m.DisplayName, Probability: logits[i] / total, Similarity: similarity(pooled, m.Counts), Score: sum[i], Family: family, FamilyName: a.Families[familyIdx[family]].DisplayName})
	}
	sort.SliceStable(a.Results, func(i, j int) bool { return a.Results[i].Probability > a.Results[j].Probability })
	for _, r := range a.Results {
		a.Families[familyIdx[r.Family]].Probability += r.Probability
	}
	for i := range a.Results {
		r := &a.Results[i]
		r.Conditional = r.Probability / a.Families[familyIdx[r.Family]].Probability
	}
	winner := a.Families[0]
	for _, f := range a.Families {
		if f.Probability > winner.Probability {
			winner = f
		}
	}
	a.FamilyPrediction = winner.Family
	a.FamilyName = winner.DisplayName
	a.FamilyProbability = winner.Probability
	a.Prediction = a.Results[0].Model
	a.PredictionName = a.Results[0].DisplayName
	a.Probability = a.Results[0].Probability
	return a, nil
}
