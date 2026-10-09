package usage

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSchemaStartupSurvivesUncleanExit(t *testing.T) {
	for _, layout := range []string{"fresh", "existing", "existing_without_generation", "existing_without_buffering"} {
		t.Run(layout, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.duckdb")
			cmd := exec.Command(os.Args[0], "-test.run=^TestUsageSchemaCrashHelper$")
			cmd.Env = append(os.Environ(), "AETHERRELAY_USAGE_CRASH_DB="+path, "AETHERRELAY_USAGE_CRASH_LAYOUT="+layout)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("unclean startup: %v %s", err, output)
			}
			store, err := OpenDuckDB(testCfg(path))
			if err != nil {
				t.Fatalf("restart with committed WAL: %v", err)
			}
			defer store.Close()
			dash, err := store.Dashboard(context.Background(), UsageFilter{AllTime: true})
			if err != nil || dash.Summary.TPS == nil || *dash.Summary.TPS != 25 || dash.Summary.TPSSamples != 1 || dash.Summary.TPSBufferedSamples != 1 {
				t.Fatalf("generation WAL recovery: %+v %v", dash, err)
			}
			var count int
			if err = store.db.QueryRow("SELECT count(*) FROM usage_events WHERE event_id='committed'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("committed event lost: count=%d err=%v", count, err)
			}
		})
	}
}

func TestUsageSchemaCrashHelper(t *testing.T) {
	path := os.Getenv("AETHERRELAY_USAGE_CRASH_DB")
	if path == "" {
		return
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if layout := os.Getenv("AETHERRELAY_USAGE_CRASH_LAYOUT"); layout != "fresh" {
		if err = initializeSchema(ctx, db); err != nil {
			t.Fatal(err)
		}
		if layout == "existing_without_generation" {
			if _, err = db.Exec(`DROP TABLE usage_generation`); err != nil {
				t.Fatal(err)
			}
		}
		if layout == "existing_without_buffering" {
			if _, err = db.Exec(`DROP TABLE usage_generation_buffering`); err != nil {
				t.Fatal(err)
			}
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = sql.Open("duckdb", path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = initializeSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO usage_events(event_id,api_key_id,started_at,usage_date,state)
VALUES ('committed','key',now(),current_date,'started')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_events(event_id,api_key_id,started_at,completed_at,usage_date,state,http_status,outcome,output_tokens,total_tokens)
VALUES ('generated','key',now()-INTERVAL 1 SECOND,now(),current_date,'completed',200,'success',25,25)`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_generation VALUES ('generated',now()-INTERVAL 1 SECOND,1000000000,false)`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_generation_buffering VALUES ('generated')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
