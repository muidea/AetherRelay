package modeltrace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	data, e := os.ReadFile("assets/vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/unified_bank.json")
	if e != nil {
		t.Fatal(e)
	}
	engine, e := New(data)
	if e != nil {
		t.Fatal(e)
	}
	return engine
}
func testRequest() Request {
	return Request{Outputs: []Output{{Text: "[" + strings.Repeat("1,", 299) + "1]", ExpectedCount: 300}}, RequestedModel: "gpt-6.1-sol"}
}

func TestEngineOwnsBankAndReturnedValues(t *testing.T) {
	data, e := os.ReadFile("assets/vendor/modeltrace/6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2/unified_bank.json")
	if e != nil {
		t.Fatal(e)
	}
	engine, e := New(data)
	if e != nil {
		t.Fatal(e)
	}
	for i := range data {
		data[i] = 0
	}
	first, e := engine.Score(context.Background(), testRequest())
	if e != nil {
		t.Fatal(e)
	}
	expected := first.Analysis.Results[0]
	if !first.ClosedSet || first.RequestedModelInBank || !engine.HasModel("gpt-6-astra") {
		t.Fatal("incorrect coverage")
	}
	first.Analysis.Results[0].Model = "mutated"
	first.Analysis.Families[0].Probability = -1
	first.Snapshot.BankSHA256 = "mutated"
	again, e := engine.Score(context.Background(), testRequest())
	if e != nil {
		t.Fatal(e)
	}
	if again.Analysis.Results[0] != expected || again.Snapshot.BankSHA256 == "mutated" || again.Analysis.Families[0].Probability < 0 {
		t.Fatal("mutable state escaped engine")
	}
}
func TestEngineConcurrency(t *testing.T) {
	engine := newTestEngine(t)
	want, e := engine.Score(context.Background(), testRequest())
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := engine.Score(context.Background(), testRequest())
			if e != nil {
				t.Error(e)
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Error("concurrent result differs")
			}
			got.Analysis.Results[0].Score = 0
		}()
	}
	wg.Wait()
}
func TestEnginePoliciesAndErrors(t *testing.T) {
	engine := newTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := engine.Score(ctx, testRequest()); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	var zero Engine
	for _, engine := range []*Engine{nil, &zero} {
		if _, e := engine.Score(context.Background(), testRequest()); !errors.Is(e, ErrUninitialized) {
			t.Fatal(e)
		}
	}
	if _, e := engine.Score(nil, testRequest()); !errors.Is(e, ErrInvalidRequest) {
		t.Fatal(e)
	}
	for _, req := range []Request{{}, {Parser: "unknown", Outputs: testRequest().Outputs}, {Outputs: []Output{{ExpectedCount: -1}}}, {Outputs: []Output{{Text: strings.Repeat("x", MaxTextBytes+1), ExpectedCount: 300}}}, {Outputs: make([]Output, MaxOutputs+1)}} {
		if _, e := engine.Score(context.Background(), req); !errors.Is(e, ErrInvalidRequest) {
			t.Fatalf("expected invalid request, got %v", e)
		}
	}
	req := testRequest()
	req.Outputs[0].Text = strings.Trim(req.Outputs[0].Text, "[]")
	if _, e := engine.Score(context.Background(), req); e == nil {
		t.Fatal("default parser silently relaxed")
	}
	req.Parser = CoreCompatible
	if _, e := engine.Score(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	req.Outputs = []Output{{Text: "refusal", ExpectedCount: 300}}
	if _, e := engine.Score(context.Background(), req); !errors.Is(e, ErrNoUsableOutputs) {
		t.Fatal(e)
	}
	req.Parser = GuardStrict
	_, e := engine.Score(context.Background(), req)
	var probe *ProbeError
	if !errors.As(e, &probe) {
		t.Fatalf("missing structured parse error: %v", e)
	}
	if _, e := New([]byte(`{}`)); !errors.Is(e, ErrAssetInvalid) {
		t.Fatal(e)
	}
}

func TestCustomBankSnapshot(t *testing.T) {
	b := testBank(t)
	b.Calibration["1"] = calibrationDefinition{Beta: 2, Accuracy: .5}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := New(data)
	if err != nil {
		t.Fatal(err)
	}
	reference := newTestEngine(t)
	if custom.Snapshot().BankSHA256 == reference.Snapshot().BankSHA256 {
		t.Fatal("custom bank falsely claimed reference digest")
	}
	r, err := custom.Score(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if r.Analysis.Calibration.Beta != 2 {
		t.Fatal("did not use custom bank")
	}
}
