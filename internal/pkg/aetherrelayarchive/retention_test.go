package archive

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	retention "aetherrelay/internal/pkg/aetherrelayretention"
)

func TestAgeRetentionAtStartupAndIdleProtectsActive(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	cutoff := retention.Cutoff(now)
	for i, at := range []time.Time{cutoff.Add(-time.Hour), cutoff.Add(time.Hour), now} {
		dir := filepath.Join(root, "key", []string{"000001", "000002", "000003"}[i])
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(Metadata{StartedAt: at})
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewRecorderOptions(root, RecorderOptions{MaxRounds: 500, ScopeByAPIKey: true})
	if err != nil {
		t.Fatal(err)
	}
	assertDirMissing(t, root, "key/000001")
	assertDirExists(t, root, "key/000002")
	active, err := r.StartForAPIKey("key")
	if err != nil {
		t.Fatal(err)
	}
	old := now.AddDate(-1, 0, 0)
	// Missing metadata uses mtime, but an active round must remain protected.
	if err := os.Chtimes(active.Dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := r.Prune(context.Background(), now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertDirMissing(t, root, "key/000002")
	assertDirExists(t, root, "key/000003")
	if _, err := os.Stat(active.Dir); err != nil {
		t.Fatal("deleted active archive", err)
	}
	if err := active.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := r.Prune(context.Background(), now.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active.Dir); !os.IsNotExist(err) {
		t.Fatal("expired orphan archive retained", err)
	}
}
