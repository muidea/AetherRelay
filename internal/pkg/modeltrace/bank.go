package modeltrace

import (
	"encoding/json"
	"fmt"
	"math"
)

func loadBank(data []byte) (*bank, error) {
	var b bank
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAssetInvalid, err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAssetInvalid, err)
	}
	return &b, nil
}
func (b *bank) Validate() error {
	if b == nil {
		return fmt.Errorf("nil bank")
	}
	if b.Schema != "robust-number-fingerprint-bank" || len(b.Models) == 0 || len(b.Models) != len(b.Robust.Order) {
		return fmt.Errorf("schema or model dimension mismatch")
	}
	seen := map[string]bool{}
	for i, m := range b.Models {
		if m.ID == "" || seen[m.ID] || m.ID != b.Robust.Order[i] {
			return fmt.Errorf("duplicate ID or model order mismatch")
		}
		seen[m.ID] = true
		if err := vector(m.Counts, 355, false); err != nil {
			return err
		}
		total := 0.
		for _, v := range m.Counts {
			if v < 0 {
				return fmt.Errorf("negative counts")
			}
			total += v
		}
		if total <= 0 || math.IsInf(total, 0) {
			return fmt.Errorf("invalid count total")
		}
	}
	for _, key := range []string{"1", "2", "3"} {
		c, ok := b.Calibration[key]
		if !ok || !finite(c.Beta) || c.Beta <= 0 || !finite(c.Accuracy) || c.Accuracy < 0 || c.Accuracy > 1 {
			return fmt.Errorf("invalid calibration %s", key)
		}
	}
	if err := artifact(b.Robust.Hellinger, 355, len(b.Models), false); err != nil {
		return err
	}
	if a := b.Robust.Ordered; a != nil {
		if !finite(a.Weight) || a.Weight < 0 || a.Weight > 1 {
			return fmt.Errorf("invalid ordered weight")
		}
		if a.Weight != 0 {
			return artifact(*a, 74, len(b.Models), true)
		}
	}
	return nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func vector(v []float64, n int, positive bool) error {
	if len(v) != n {
		return fmt.Errorf("vector dimension: got %d want %d", len(v), n)
	}
	for _, x := range v {
		if !finite(x) || (positive && x <= 0) {
			return fmt.Errorf("nonfinite value or invalid scale")
		}
	}
	return nil
}
func artifact(a artifactDefinition, d, n int, ordered bool) error {
	if err := vector(a.Mean, d, false); err != nil {
		return err
	}
	if err := vector(a.Scale, d, true); err != nil {
		return err
	}
	if len(a.Centroids) != n {
		return fmt.Errorf("centroid count mismatch")
	}
	for _, rows := range [][][]float64{a.Centroids, a.Basis} {
		for _, v := range rows {
			if err := vector(v, d, false); err != nil {
				return err
			}
		}
	}
	if ordered && len(a.Environments) == 0 {
		return fmt.Errorf("missing ordered environments")
	}
	for _, rows := range a.Environments {
		if len(rows) != n {
			return fmt.Errorf("environment dimension mismatch")
		}
		for _, v := range rows {
			if err := vector(v, d, false); err != nil {
				return err
			}
		}
	}
	return nil
}
