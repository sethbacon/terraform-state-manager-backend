package db_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db"
)

// The migration helpers leaked a pooled connection per call.
//
// postgres.WithInstance checks out a *sql.Conn and holds it for the driver's
// lifetime; newMigrator never released it and no caller closed the migrator, so
// every call permanently consumed one slot of the caller's MaxOpenConns.
// cmd/server/main.go calls RunMigrations AND GetMigrationVersion with the
// long-lived application pool at startup, so two slots were lost for the life
// of the process.
//
// The obvious fix is a trap worth naming: postgres.WithInstance records the
// *sql.DB on the driver, so driver.Close()/m.Close() closes the caller's shared
// pool. "defer m.Close()" would have closed the application pool at startup.
// newMigrator now borrows a connection explicitly and uses
// postgres.WithConnection, which leaves that field nil.
//
// Third independent copy of this defect in the suite: terraform-suite-identity
// fixed it in #139, terraform-registry-backend in #788, and this repo carried
// its own copy — which is why neither of those fixes reached it.

// TestMigrationHelpers_DoNotUseWithInstance is the re-runnable signature for the
// defect, and the half that runs everywhere — no database required.
//
// It is the guard that matters most for this class: the defect has now been
// written three times independently, so what has to be prevented is the
// pattern reappearing, not one call site regressing.
func TestMigrationHelpers_DoNotUseWithInstance(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("no .go sources found — this guard is not scanning anything")
	}

	var scanned int
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			// Skip prose: the fix is explained in a comment that names the
			// symbol on purpose.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "postgres.WithInstance(") {
				t.Errorf("%s:%d uses postgres.WithInstance, which records the caller's "+
					"*sql.DB on the driver — the driver then holds a pooled connection "+
					"for its lifetime AND its Close() closes the caller's pool. Use "+
					"newMigrator/closeMigrator (postgres.WithConnection over a borrowed "+
					"connection) instead.", path, i+1)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test sources — the guard is vacuous")
	}
}

// TestGetMigrationVersion_ReturnsItsConnectionToThePool uses a pool of exactly
// one connection, so a leak is a deadlock rather than a statistic.
//
// Asserted against a context deadline rather than db.Stats(), because the
// failure mode is that the next query BLOCKS waiting for a connection that will
// never come back. Stats alone would report a plausible InUse count and say
// nothing about whether the pool still works.
//
// Set TSM_TEST_DATABASE_URL to run this (any reachable Postgres; the helper
// only reads golang-migrate's version table).
func TestGetMigrationVersion_ReturnsItsConnectionToThePool(t *testing.T) {
	dsn := os.Getenv("TSM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TSM_TEST_DATABASE_URL not set — needs a reachable Postgres")
	}

	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer pool.Close()

	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		t.Skipf("Postgres not reachable at TSM_TEST_DATABASE_URL: %v", err)
	}

	// More than once: a per-call leak with MaxOpenConns(1) blocks on the second
	// call, which is itself the regression signal.
	for i := 0; i < 3; i++ {
		if _, _, err := db.GetMigrationVersion(pool); err != nil {
			t.Fatalf("GetMigrationVersion call %d: %v", i+1, err)
		}
	}

	queryCtx, queryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer queryCancel()

	var one int
	if err := pool.QueryRowContext(queryCtx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("pool is unusable after GetMigrationVersion — the borrowed "+
			"connection was never returned: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}

	if inUse := pool.Stats().InUse; inUse != 0 {
		t.Errorf("pool reports %d connection(s) still in use after the helper returned", inUse)
	}
}
