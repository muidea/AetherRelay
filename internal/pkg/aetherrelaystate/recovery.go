package aetherrelaystate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type RecoveryReport struct {
	BackupDir string
	TableRows map[string]int64
}

type recoveryTable struct {
	Rows     int64
	Checksum uint64
}

// RecoverWAL is an explicit offline operation. The caller must stop all writers.
// It preserves both original files before replay; it never discards the WAL.
// An initialized in-memory catalog provides the default database that DuckDB's
// affected WAL replay path expects while attaching the persistent database.
func RecoverWAL(ctx context.Context, path string) (report RecoveryReport, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return report, err
	}
	for _, file := range []string{path, path + ".wal"} {
		info, statErr := os.Lstat(file)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return report, fmt.Errorf("recovery requires nonempty regular database and WAL files: %s", file)
		}
	}
	report.BackupDir, err = os.MkdirTemp(filepath.Dir(path), filepath.Base(path)+".recovery-backup-")
	if err != nil {
		return report, fmt.Errorf("create recovery backup: %w", err)
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("recovery failed; original backup at %s: %w", report.BackupDir, err)
		}
	}()
	for _, file := range []string{path, path + ".wal"} {
		if err = copyRecoveryFile(file, filepath.Join(report.BackupDir, filepath.Base(file))); err != nil {
			return report, err
		}
	}
	for _, file := range []string{path, path + ".wal"} {
		var originalHash, backupHash [32]byte
		originalHash, err = recoveryFileHash(file)
		if err != nil {
			return report, err
		}
		backupHash, err = recoveryFileHash(filepath.Join(report.BackupDir, filepath.Base(file)))
		if err != nil {
			return report, err
		}
		if originalHash != backupHash {
			return report, fmt.Errorf("database changed during backup; stop all writers before recovery")
		}
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return report, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"SET memory_limit = '256MB'", "SET threads = 2",
		"SET autoinstall_known_extensions = false", "SET autoload_known_extensions = false",
		"ATTACH '" + strings.ReplaceAll(path, "'", "''") + "' AS aetherrelay_recovery",
		"USE aetherrelay_recovery", "SET enable_external_access = false",
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			return report, err
		}
	}
	before, err := snapshotRecoveryTables(ctx, db)
	if err != nil {
		return report, err
	}
	if len(before) == 0 {
		return report, fmt.Errorf("recovered database has no application tables")
	}
	if _, err = db.ExecContext(ctx, "CHECKPOINT aetherrelay_recovery"); err != nil {
		return report, err
	}
	if err = db.Close(); err != nil {
		return report, err
	}
	// Verify the ordinary startup path and all table row fingerprints after the
	// checkpoint, with the same driver that the running service will use.
	direct, err := sql.Open("duckdb", path)
	if err != nil {
		return report, err
	}
	defer direct.Close()
	after, err := snapshotRecoveryTables(ctx, direct)
	if err != nil {
		return report, err
	}
	if len(before) != len(after) {
		return report, fmt.Errorf("table contents changed across recovery checkpoint")
	}
	for table, expected := range before {
		if actual, ok := after[table]; !ok || actual != expected {
			return report, fmt.Errorf("table contents changed across recovery checkpoint")
		}
	}
	if err = direct.Close(); err != nil {
		return report, err
	}
	report.TableRows = make(map[string]int64, len(after))
	for table, snapshot := range after {
		report.TableRows[table] = snapshot.Rows
	}
	return report, nil
}

func snapshotRecoveryTables(ctx context.Context, db *sql.DB) (map[string]recoveryTable, error) {
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.tables
WHERE table_catalog = current_database() AND table_schema = 'main' AND table_type = 'BASE TABLE'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make(map[string]recoveryTable, len(tables))
	for _, table := range tables {
		var snapshot recoveryTable
		query := `SELECT count(*), coalesce(bit_xor(hash(t)), 0) FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" AS t`
		if err = db.QueryRowContext(ctx, query).Scan(&snapshot.Rows, &snapshot.Checksum); err != nil {
			return nil, err
		}
		result[table] = snapshot
	}
	return result, nil
}

func copyRecoveryFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func recoveryFileHash(path string) (result [32]byte, err error) {
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}
