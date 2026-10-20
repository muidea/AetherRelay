package assets_test

import (
	"aetherrelay/internal/pkg/modeltrace"
	"aetherrelay/internal/pkg/modeltrace/assets"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const Tolerance = 1e-10

type Fixture struct {
	Version    string        `json:"fixture_version"`
	Commit     string        `json:"upstream_commit"`
	BankHash   string        `json:"bank_sha256"`
	OracleNode string        `json:"oracle_node"`
	Cases      []NumericCase `json:"numeric"`
	Parser     []ParserCase  `json:"parser"`
	Runs       []RunCase     `json:"runs"`
}
type NumericCase struct {
	Name     string              `json:"name"`
	Outputs  []modeltrace.Output `json:"outputs"`
	Variant  string              `json:"variant"`
	Expected json.RawMessage     `json:"expected"`
	Error    bool                `json:"error"`
}
type ParserCase struct {
	Name       string                 `json:"name"`
	Text       *string                `json:"text"`
	Count      int                    `json:"expected_count"`
	Values     []int                  `json:"values"`
	Diagnostic *modeltrace.ProbeError `json:"diagnostic"`
}
type RunCase struct {
	Text   string `json:"text"`
	Values []int  `json:"values"`
}
type ParityResult struct {
	Total         int      `json:"total"`
	Passed        int      `json:"passed"`
	Failures      []string `json:"failures"`
	Absolute      float64  `json:"absolute_tolerance"`
	Relative      float64  `json:"relative_tolerance"`
	MaxDifference float64  `json:"max_absolute_difference"`
	FixtureHash   string   `json:"fixture_sha256"`
}

func variant(data []byte, name string) ([]byte, error) {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	var robust map[string]json.RawMessage
	if err := json.Unmarshal(b["robust"], &robust); err != nil {
		return nil, err
	}
	if name == "missing" {
		delete(robust, "ordered_blocks")
	} else if name != "" {
		for _, key := range []string{"hellinger", "ordered_blocks"} {
			var a struct {
				Mean         []float64     `json:"feature_mean"`
				Scale        []float64     `json:"feature_scale"`
				Basis        [][]float64   `json:"nuisance_basis"`
				Centroids    [][]float64   `json:"centroids"`
				Environments [][][]float64 `json:"environment_centroids,omitempty"`
				Weight       float64       `json:"weight,omitempty"`
			}
			if err := json.Unmarshal(robust[key], &a); err != nil {
				return nil, err
			}
			switch name {
			case "disabled":
				if key == "ordered_blocks" {
					a.Weight = 0
				}
			case "zero-norm", "near-zero-norm":
				a.Basis = nil
				for i := range a.Mean {
					count, total := 0., 477.5
					if key == "hellinger" {
						if i == 0 {
							count = 300
						}
					} else if i < 64 {
						total = 83
						if i%16 == 0 {
							count = 75
						}
					} else {
						total = 305
						if i == 65 {
							count = 300
						}
					}
					a.Mean[i] = math.Sqrt((count + .5) / total)
					if name == "near-zero-norm" {
						a.Mean[i] += 1e-15
					}
					a.Scale[i] = 1
				}
			case "zero", "near-zero", "ties":
				a.Basis = nil
				for i := range a.Mean {
					a.Mean[i] = 0
					a.Scale[i] = 1
				}
				for i := range a.Centroids {
					for j := range a.Centroids[i] {
						v := 0.
						if name == "near-zero" {
							v = float64((i+j)%5) * 1e-15
						}
						if name == "ties" {
							v = .1
						}
						a.Centroids[i][j] = v
					}
				}
				for _, rows := range a.Environments {
					for _, row := range rows {
						for j := range row {
							row[j] = 0
						}
					}
				}
			default:
				return nil, fmt.Errorf("unknown fixture variant")
			}
			encoded, err := json.Marshal(a)
			if err != nil {
				return nil, err
			}
			robust[key] = encoded
		}
	}
	encoded, err := json.Marshal(robust)
	if err != nil {
		return nil, err
	}
	b["robust"] = encoded
	return json.Marshal(b)
}
func Parity(dir string, bankJSON []byte) (ParityResult, error) {
	r := ParityResult{Failures: []string{}, Absolute: Tolerance, Relative: Tolerance}
	data, err := os.ReadFile(filepath.Join(dir, "oracle.json"))
	if err != nil {
		return r, err
	}
	r.FixtureHash = parityDigest(data)
	var f Fixture
	if err = json.Unmarshal(data, &f); err != nil {
		return r, err
	}
	if f.Version != "1" || f.Commit != assets.Commit || f.BankHash != assets.BankHash || f.OracleNode == "" || len(f.Cases) == 0 || len(f.Parser) == 0 || len(f.Runs) == 0 {
		return r, fmt.Errorf("invalid or empty oracle fixture")
	}
	check := func(name string, actual, expected any) {
		r.Total++
		if e := compare(actual, expected, "", &r.MaxDifference); e != nil {
			r.Failures = append(r.Failures, name+": "+e.Error())
		} else {
			r.Passed++
		}
	}
	for _, c := range f.Cases {
		data, e := variant(bankJSON, c.Variant)
		if e != nil {
			return r, e
		}
		engine, e := modeltrace.New(data)
		if e != nil {
			return r, e
		}
		evaluated, e := engine.Score(context.Background(), modeltrace.Request{Outputs: c.Outputs, Parser: modeltrace.CoreCompatible})
		var actual *modeltrace.Analysis
		if evaluated != nil {
			actual = evaluated.Analysis
		}
		if c.Error {
			check(c.Name, e != nil, true)
			continue
		}
		if e != nil {
			r.Total++
			r.Failures = append(r.Failures, c.Name+": analysis error")
			continue
		}
		a, _ := json.Marshal(actual)
		var av, ev any
		_ = json.Unmarshal(a, &av)
		if e = json.Unmarshal(c.Expected, &ev); e != nil {
			return r, e
		}
		check(c.Name, av, ev)
	}
	for _, c := range f.Parser {
		values, e := modeltrace.ValidateOutput(c.Text, c.Count)
		var actual any = values
		if e != nil {
			actual = e
		}
		var expected any = c.Values
		if c.Diagnostic != nil {
			expected = c.Diagnostic
		}
		a, _ := json.Marshal(actual)
		expectedJSON, _ := json.Marshal(expected)
		var av, ev any
		_ = json.Unmarshal(a, &av)
		_ = json.Unmarshal(expectedJSON, &ev)
		check(c.Name, av, ev)
	}
	for i, c := range f.Runs {
		check(fmt.Sprintf("run-%d", i), toJSON(modeltrace.ParseNumbers(c.Text)), toJSON(c.Values))
	}
	return r, nil
}
func toJSON(v any) any { d, _ := json.Marshal(v); var out any; _ = json.Unmarshal(d, &out); return out }
func compare(a, b any, path string, maxdiff *float64) error {
	switch e := b.(type) {
	case float64:
		v, ok := a.(float64)
		if !ok || !finite(v) {
			return fmt.Errorf("%s: invalid number", path)
		}
		diff := math.Abs(v - e)
		*maxdiff = math.Max(*maxdiff, diff)
		if diff > Tolerance+Tolerance*math.Abs(e) {
			return fmt.Errorf("%s: number differs", path)
		}
	case map[string]any:
		v, ok := a.(map[string]any)
		if !ok || len(v) != len(e) {
			return fmt.Errorf("%s: object shape differs", path)
		}
		for k, w := range e {
			if err := compare(v[k], w, path+"."+k, maxdiff); err != nil {
				return err
			}
		}
	case []any:
		v, ok := a.([]any)
		if !ok || len(v) != len(e) {
			return fmt.Errorf("%s: array length differs", path)
		}
		for i, w := range e {
			if err := compare(v[i], w, fmt.Sprintf("%s[%d]", path, i), maxdiff); err != nil {
				return err
			}
		}
	default:
		if a != b {
			return fmt.Errorf("%s: value differs", path)
		}
	}
	return nil
}
func finite(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }

func parityDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func TestCopiedPoCOracleParity(t *testing.T) {
	bank, err := os.ReadFile("vendor/modeltrace/" + assets.Commit + "/unified_bank.json")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Parity("../testdata", bank)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 66 || result.Passed != result.Total {
		t.Fatalf("parity %d/%d: %v", result.Passed, result.Total, result.Failures)
	}
	t.Logf("parity %d/%d; max abs difference %.17g", result.Passed, result.Total, result.MaxDifference)
}
