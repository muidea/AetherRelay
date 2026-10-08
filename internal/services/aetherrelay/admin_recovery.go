package aetherrelay

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"aetherrelay/internal/pkg/aetherrelaystate"
)

func runAdminRecoverState(args []string) int {
	fs := flag.NewFlagSet("AetherRelay admin recover-state", flag.ContinueOnError)
	path := fs.String("database", "", "database file path; stop the service before running recovery")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(*path) == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: AetherRelay admin recover-state --database <database.duckdb>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	report, err := aetherrelaystate.RecoverWAL(ctx, *path)
	if err != nil {
		// DuckDB may attach a large native stack trace. Keep the diagnostic bounded.
		fmt.Fprintln(os.Stderr, strings.SplitN(err.Error(), "\n", 2)[0])
		return 1
	}
	fmt.Println("Recovery completed; original database and WAL backup:", report.BackupDir)
	var tables []string
	for table := range report.TableRows {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		fmt.Printf("table=%s rows=%d\n", table, report.TableRows[table])
	}
	return 0
}
