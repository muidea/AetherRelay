package usage

import (
	clientaccess "aetherrelay/internal/pkg/aetherrelayclientaccess"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestClientKeyDeletionSurvivesReopenAndRejectsMutation(t *testing.T) {
	ctx := context.Background()
	cfg := testCfg(filepath.Join(t.TempDir(), "usage.duckdb"))
	s, err := OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	record := ClientAPIKeyRecord{ID: "deleting", Hash: "sha256:test", Enabled: true, CreatedAt: time.Now().UTC(), ProviderAccess: clientaccess.All()}
	if err := s.CreateClientAPIKey(ctx, record); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.BeginClientAPIKeyDeletion(ctx, record.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenDuckDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	keys, err := s.ListClientAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := keys[record.ID]
	if got.Enabled || got.DeletingAt == nil || !got.DeletingAt.Equal(at) {
		t.Fatalf("lost deletion state: %+v", got)
	}
	for _, mutate := range []func() error{
		func() error { return s.SetClientAPIKeyEnabled(ctx, record.ID, true) },
		func() error { return s.RotateClientAPIKey(ctx, record.ID, "new", time.Now()) },
		func() error { return s.SetClientAPIKeyProviderAccess(ctx, record.ID, clientaccess.All()) },
	} {
		if err := mutate(); err == nil {
			t.Fatal("pending deletion accepted mutation")
		}
	}
	if err := s.BeginClientAPIKeyDeletion(ctx, record.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteClientAPIKey(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	keys, err = s.ListClientAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys[record.ID]; ok {
		t.Fatal("retry left deleted key")
	}
}
