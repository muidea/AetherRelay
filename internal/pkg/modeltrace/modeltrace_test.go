package modeltrace

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
)

func testBank(t *testing.T) *bank {
	t.Helper()
	data, e := os.ReadFile("assets/vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/unified_bank.json")
	if e != nil {
		t.Fatal(e)
	}
	b, e := loadBank(data)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestBankValidation(t *testing.T) {
	cases := map[string]func(*bank){"schema": func(b *bank) { b.Schema = "unknown" }, "duplicate": func(b *bank) { b.Models[1].ID = b.Models[0].ID }, "order": func(b *bank) { b.Robust.Order[0] = "bad" }, "dimension": func(b *bank) { b.Robust.Hellinger.Mean = nil }, "negative": func(b *bank) { b.Models[0].Counts[0] = -1 }, "total": func(b *bank) {
		for i := range b.Models[0].Counts {
			b.Models[0].Counts[i] = 0
		}
	}, "scale": func(b *bank) { b.Robust.Hellinger.Scale[0] = 0 }, "nan": func(b *bank) { b.Robust.Hellinger.Mean[0] = math.NaN() }, "infinity": func(b *bank) { b.Models[0].Counts[0] = math.Inf(1) }, "calibration": func(b *bank) { delete(b.Calibration, "2") }, "beta": func(b *bank) { b.Calibration["1"] = calibrationDefinition{} }, "ordered": func(b *bank) { b.Robust.Ordered.Environments = nil }, "basis": func(b *bank) { b.Robust.Hellinger.Basis = [][]float64{{1}} }, "weight": func(b *bank) { b.Robust.Ordered.Weight = 2 }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := testBank(t)
			mutate(b)
			if b.Validate() == nil {
				t.Fatal("accepted invalid bank")
			}
		})
	}
	b := testBank(t)
	b.Robust.Ordered = nil
	if e := b.Validate(); e != nil {
		t.Fatal(e)
	}
	b = testBank(t)
	b.Robust.Ordered = &artifactDefinition{}
	if e := b.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, d := range []string{`{"models": [NaN]}`, `{"models":[1e999]}`, `{} `} {
		if _, e := loadBank([]byte(d)); e == nil {
			t.Fatal("invalid JSON bank accepted")
		}
	}
}
func TestStrictDoesNotRepair(t *testing.T) {
	text := "[" + strings.Repeat("1,", 299) + "1]"
	v, e := ValidateNumbers(text, 300)
	if e != nil || len(v) != 300 {
		t.Fatal(e)
	}
	for _, bad := range []string{"Here is " + text, text + " trailing", "```json\n" + text + "\n~~~~", "[+1]", "[1.0]", "[1e2]"} {
		if _, e = ValidateNumbers(bad, 300); e == nil {
			t.Fatal("repaired invalid sample")
		}
	}
}
func TestAggregationEqualWeight(t *testing.T) {
	b := testBank(t)
	a := Output{Text: "[" + strings.Repeat("1,", 299) + "1]", ExpectedCount: 300}
	c := Output{Text: "[" + strings.Repeat("355,", 164) + "355]", ExpectedCount: 300}
	left, e := analyze(context.Background(), []Output{a}, b)
	if e != nil {
		t.Fatal(e)
	}
	right, e := analyze(context.Background(), []Output{c}, b)
	if e != nil {
		t.Fatal(e)
	}
	both, e := analyze(context.Background(), []Output{a, c}, b)
	if e != nil {
		t.Fatal(e)
	}
	lookup := func(a *Analysis) map[string]float64 {
		m := map[string]float64{}
		for _, r := range a.Results {
			m[r.Model] = r.Score
		}
		return m
	}
	l, r := lookup(left), lookup(right)
	for _, v := range both.Results {
		if math.Abs(v.Score-(l[v.Model]+r[v.Model])/2) > 1e-14 {
			t.Fatal("batch not equal weight")
		}
	}
	if both.Calibration.Queries != "2" {
		t.Fatal("wrong calibration")
	}
}
func TestFeatureBoundaries(t *testing.T) {
	for n := 0; n < 9; n++ {
		nums := make([]int, n)
		for i := range nums {
			nums[i] = 355
		}
		f := ordered(nums)
		if len(f) != 74 {
			t.Fatal("ordered dimension")
		}
		for _, v := range f {
			if !finite(v) {
				t.Fatal("nonfinite feature")
			}
		}
	}
	for _, v := range normalized([]float64{0, 0}) {
		if v != 0 {
			t.Fatal("zero norm")
		}
	}
	if standardize([]float64{1, 1})[0] != 0 {
		t.Fatal("ties")
	}
}
