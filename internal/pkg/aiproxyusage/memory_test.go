package usage

import (
	"bytes"
	"context"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestMemoryStoreConversionObservability(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	if err := store.Start(ctx, StartRecord{EventID: "memory-conversion", StartedAt: now, APIKeyID: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, CompleteRecord{
		EventID:             "memory-conversion",
		CompletedAt:         now.Add(time.Second),
		HTTPStatus:          200,
		Outcome:             "success",
		ConversionMode:      "anthropic_to_responses",
		ConversionLevel:     3,
		ConversionDegraded:  true,
		IgnoredFeatures:     []string{"thinking_output"},
		UnsupportedFeatures: []string{"images"},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := store.Events(ctx, EventFilter{UsageFilter: UsageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, PageSize: 10})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("events=%+v err=%v", page, err)
	}
	event := page.Events[0]
	if event.ConversionLevel != 3 || !event.ConversionDegraded || strings.Join(event.IgnoredFeatures, ",") != "thinking_output" || strings.Join(event.UnsupportedFeatures, ",") != "images" {
		t.Fatalf("event=%+v", event)
	}
	var output bytes.Buffer
	if err := store.ExportCSV(ctx, UsageFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, &output); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(output.Bytes())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	columns := map[string]int{}
	for index, name := range rows[0] {
		columns[name] = index
	}
	if rows[1][columns["ignored_features"]] != "thinking_output" || rows[1][columns["unsupported_features"]] != "images" {
		t.Fatalf("CSV conversion features=%v", rows[1])
	}
}
