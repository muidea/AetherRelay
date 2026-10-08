package aetherrelay

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	state "aetherrelay/internal/pkg/aetherrelaystate"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

func TestEntrySignalHelper(t *testing.T) {
	configPath := os.Getenv("AETHERRELAY_SIGNAL_TEST_CONFIG")
	if configPath == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("signal-helper", flag.ExitOnError)
	os.Args = []string{"AetherRelay", "-config", configPath}
	os.Exit(Run("signal-regression"))
}

// Exercise the real entry, signal handling, framework owners and persistent
// file, twice. The fixture includes committed usage and an opaque credential.
func TestEntrySIGTERMReopensExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	store, err := usage.OpenDuckDB(cfg.UsageStore)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Start(context.Background(), usage.StartRecord{EventID: "before-upgrade", APIKeyID: "test", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(context.Background(), usage.CompleteRecord{EventID: "before-upgrade", CompletedAt: now, HTTPStatus: 200, Outcome: "success", CachedInputTokensKnown: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	documents, err := state.Open(cfg.State.Database, "256MB", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := documents.ReplaceSecureDocuments("upgrade-fixture", []state.SecureDocumentRow{{ID: "credential", Payload: []byte("opaque-sealed-fixture")}}); err != nil {
		t.Fatal(err)
	}
	if err := documents.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		listener.Close()
		configPath := filepath.Join(dir, "config.yaml")
		content := fmt.Sprintf("server:\n  listen_addr: %s\nstate:\n  dir: %s\n  database: %s\nchatgpt_web:\n  temporary_chat:\n    enabled: true\n", addr, dir, filepath.Base(cfg.State.Database))
		if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(dir, fmt.Sprintf("run-%d.log", attempt))
		logFile, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEntrySignalHelper$")
		cmd.Env = append(os.Environ(), "AETHERRELAY_SIGNAL_TEST_CONFIG="+configPath, "AETHERRELAY_CREDENTIAL_KEY=CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk=")
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err := cmd.Start(); err != nil {
			cancel()
			logFile.Close()
			t.Fatal(err)
		}
		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
		ready := false
		for ctx.Err() == nil {
			response, err := client.Get("http://" + addr + "/healthz")
			if err == nil {
				response.Body.Close()
				ready = response.StatusCode == 200
			}
			if ready {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if ready {
			err = cmd.Process.Signal(syscall.SIGTERM)
		}
		waitErr := cmd.Wait()
		cancel()
		logFile.Close()
		client.CloseIdleConnections()
		output, _ := os.ReadFile(logPath)
		if !ready || err != nil || waitErr != nil || !strings.Contains(string(output), "AetherRelay shutdown completed") {
			t.Fatalf("attempt=%d ready=%v signal=%v wait=%v log=%s", attempt, ready, err, waitErr, output)
		}
	}
	db, err := sql.Open("duckdb", cfg.State.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM usage_events WHERE event_id='before-upgrade' AND cached_input_tokens_known=true").Scan(&count); err != nil || count != 1 {
		t.Fatalf("usage changed: count=%d err=%v", count, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM secure_documents WHERE scope='upgrade-fixture' AND id='credential'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("credential changed: count=%d err=%v", count, err)
	}

}
