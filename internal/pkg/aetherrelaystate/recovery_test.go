package aetherrelaystate

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverWALPreservesCommittedContentsAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state's.duckdb")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryCrashHelper$")
	cmd.Env = append(os.Environ(), "AETHERRELAY_RECOVERY_CRASH_DB="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash fixture: %v %s", err, output)
	}
	databaseHash, err := recoveryFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	walHash, err := recoveryFileHash(path + ".wal")
	if err != nil {
		t.Fatal(err)
	}
	// This fixture reproduces the production assertion with the pinned driver.
	if db, err := sql.Open("duckdb", path); err == nil {
		db.Close()
		t.Fatal("fixture must require the alternate WAL replay path")
	} else if !strings.Contains(err.Error(), "no default database set") {
		t.Fatalf("unexpected recovery failure: %v", err)
	}
	report, err := RecoverWAL(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.TableRows["evidence"] != 2 {
		t.Fatalf("recovered rows: %+v", report)
	}
	for file, want := range map[string][32]byte{filepath.Base(path): databaseHash, filepath.Base(path) + ".wal": walHash} {
		backup := filepath.Join(report.BackupDir, file)
		got, err := recoveryFileHash(backup)
		if err != nil || got != want {
			t.Fatalf("backup differs: %s %v", file, err)
		}
		info, err := os.Stat(backup)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("backup permissions: %v", err)
		}
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var contents string
	if err = db.QueryRow("SELECT string_agg(payload, ',' ORDER BY id) FROM evidence").Scan(&contents); err != nil || contents != "before,after" {
		t.Fatalf("WAL data lost: contents=%q err=%v", contents, err)
	}
	if _, err = RecoverWAL(context.Background(), filepath.Join(t.TempDir(), "missing.duckdb")); err == nil {
		t.Fatal("recovery must not create a missing database")
	}
}

func TestRecoveryCrashHelper(t *testing.T) {
	path := os.Getenv("AETHERRELAY_RECOVERY_CRASH_DB")
	if path == "" {
		return
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE evidence (id INTEGER PRIMARY KEY, payload VARCHAR, CHECK (length(payload)>0))",
		"INSERT INTO evidence VALUES (1, 'before')", "CHECKPOINT",
		"ALTER TABLE evidence ADD COLUMN known BOOLEAN DEFAULT FALSE",
		"INSERT INTO evidence(id, payload) VALUES (2, 'after')",
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate process loss after commit; do not run DB.Close/checkpoint.
	os.Exit(0)
}
