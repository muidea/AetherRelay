package usagetarget

import (
	"context"
	"sync"
	"testing"

	codexmodel "aetherrelay/internal/pkg/aetherrelaycodexmodel"
)

func TestCodexEvidenceFollowsFinalAccountAndResetsSameAccountRetry(t *testing.T) {
	ctx := WithTracker(context.Background())
	Record(ctx, "codexoauth", "first", "first@example.com")
	BeginCodexAttempt(ctx, []byte(`{"model":"gpt-6.1-sol"}`))
	ObserveCodex(ctx, []byte(`{"type":"response.completed","response":{"model":"gpt-6-luna"}}`))
	if !Snapshot(ctx).ModelSubstituted {
		t.Fatal("missing substitution")
	}
	BeginCodexAttempt(ctx, []byte(`{"model":"gpt-6.1-sol"}`))
	if Snapshot(ctx).ResponseModel != "" || Snapshot(ctx).AccountID != "first" {
		t.Fatal("same-account retry retained evidence")
	}
	Record(ctx, "codexoauth", "last", "last@example.com")
	SetCodexModels(ctx, codexmodel.New("gpt-6.1-sol", "gpt-6.1-sol"))
	if got := Snapshot(ctx); got.ModelSubstituted || got.AccountLabel != "last@example.com" {
		t.Fatal(got)
	}
	Record(ctx, "chatgptweb", "web", "web@example.com")
	ObserveCodex(ctx, []byte(`{"type":"response.completed","response":{"model":"gpt-6-luna"}}`))
	SetCodexModels(ctx, codexmodel.New("gpt-6.1-sol", "gpt-6-luna"))
	if Snapshot(ctx).ResponseModel != "" {
		t.Fatal("Web pool participated")
	}
	Record(ctx, "ordinary", "", "")
	if Snapshot(ctx).AccountID != "" {
		t.Fatal("provider fallback retained account")
	}
}

func TestConcurrentCodexObservationSnapshot(t *testing.T) {
	ctx := WithTracker(context.Background())
	Record(ctx, "codexoauth", "slot", "slot@example.com")
	BeginCodexAttempt(ctx, []byte(`{"model":"gpt-6.1-sol"}`))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				ObserveCodex(ctx, []byte(`{"type":"response.completed","response":{"model":"gpt-6-luna"}}`))
				_ = Snapshot(ctx)
			}
		}()
	}
	wg.Wait()
	if got := Snapshot(ctx); !got.ModelSubstituted || got.AccountID != "slot" {
		t.Fatal(got)
	}
}
