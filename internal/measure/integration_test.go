package measure

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/sqlparse"
)

// The integration tests need a PostgreSQL server they may create and drop
// databases on: MIGRATE_SAFE_TEST_DSN=postgres://postgres:pg@localhost:5432/postgres

func testServer(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MIGRATE_SAFE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_SAFE_TEST_DSN not set")
	}
	return dsn
}

// demoDB creates a fresh demo database with rows orders and returns its URL.
func demoDB(t *testing.T, server string, rows int) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, server)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("ms_test_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), server)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	src, err := os.ReadFile("../../examples/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := sqlparse.Split(strings.ReplaceAll(string(src), ":rows", fmt.Sprint(rows)))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(stmts, sqlparse.Statement{SQL: "VACUUM ANALYZE"}) {
		if _, err := conn.Exec(ctx, s.SQL); err != nil {
			t.Fatalf("schema line %d: %v", s.Line, err)
		}
	}
	return u.String()
}

func readSQL(t *testing.T, path string) []sqlparse.Statement {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := sqlparse.Split(string(src))
	if err != nil {
		t.Fatal(err)
	}
	return stmts
}

func quiet(t *testing.T) func(string, ...any) {
	return func(f string, a ...any) { t.Logf(f, a...) }
}

// TestCorpusAgreesWithServer runs ~60 statements of every kind the linter
// knows and fails on any difference between its predictions and what the
// server did.
func TestCorpusAgreesWithServer(t *testing.T) {
	db := demoDB(t, testServer(t), 20000)
	run, err := Measure(context.Background(), Config{DSN: db, InPlace: true, Warmup: time.Millisecond, Tail: time.Millisecond, Log: quiet(t)},
		readSQL(t, "testdata/corpus.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Failed != "" {
		t.Fatalf("corpus failed: %s", run.Failed)
	}
	dis, un := Agree(run)
	for _, d := range dis {
		t.Errorf("line %d: %s", d.Line, d.What)
	}
	if len(un) > 0 {
		t.Errorf("lines not analysed by lint: %v", un)
	}
	t.Logf("PostgreSQL %s: %d statements agree", run.Server, len(run.Statements))
}

func loadWorkload(t *testing.T) *Workload {
	src, err := os.ReadFile("../../examples/workload.sql")
	if err != nil {
		t.Fatal(err)
	}
	w, err := ParseWorkload(string(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetWorkers("read=2,write=2,insert=1"); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestStallIsMeasured checks the measurement itself: a migration that holds
// ACCESS EXCLUSIVE for one second must show reads and writes stuck for
// about that long, in both the probes and the workload.
func TestStallIsMeasured(t *testing.T) {
	db := demoDB(t, testServer(t), 5000)
	stmts, _ := sqlparse.Split("BEGIN; LOCK TABLE orders IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(1); COMMIT;")
	run, err := Measure(context.Background(), Config{DSN: db, Workload: loadWorkload(t), Warmup: 500 * time.Millisecond, Log: quiet(t)}, stmts)
	if err != nil {
		t.Fatal(err)
	}
	b := run.BlockedTotal()["orders"]
	for name, d := range map[string]time.Duration{
		"probe reads": b.Reads, "probe writes": b.Writes,
		"longest read": run.Overall["read"].Max, "longest write": run.Overall["write"].Max,
	} {
		if d < 900*time.Millisecond || d > 2*time.Second {
			t.Errorf("%s = %s, want about 1s", name, d)
		}
	}
	if b := run.BlockedTotal()["accounts"]; b.Reads > 0 || b.Writes > 0 {
		t.Errorf("accounts should not be blocked: %+v", b)
	}
	if run.Baseline["read"].Max > 500*time.Millisecond {
		t.Errorf("baseline already slow: %+v", run.Baseline["read"])
	}
}

// TestExamplesAgree measures every example, as written and as rewritten,
// and checks the linter against each measurement.
func TestExamplesAgree(t *testing.T) {
	server := testServer(t)
	if testing.Short() {
		t.Skip("slow: runs every example")
	}
	db := demoDB(t, server, 50000)
	dirs, _ := filepath.Glob("../../examples/migrations/*")
	for _, dir := range dirs {
		for _, variant := range []string{"unsafe", "safe"} {
			name := filepath.Base(dir) + "/" + variant
			t.Run(name, func(t *testing.T) {
				run, err := Measure(context.Background(), Config{DSN: db, Workload: loadWorkload(t), Warmup: 300 * time.Millisecond, Log: quiet(t)},
					readSQL(t, filepath.Join(dir, variant+".sql")))
				if err != nil {
					t.Fatal(err)
				}
				if run.Failed != "" {
					t.Fatalf("failed: %s", run.Failed)
				}
				dis, un := Agree(run)
				for _, d := range dis {
					t.Errorf("line %d: %s", d.Line, d.What)
				}
				if len(un) > 0 {
					t.Errorf("not analysed: %v", un)
				}
				worst := lint.OK
				for _, r := range run.Lint {
					worst = max(worst, r.Severity())
				}
				if variant == "unsafe" && worst < lint.Risk {
					t.Errorf("lint passes the unsafe version")
				}
				if variant == "safe" && worst >= lint.Risk {
					t.Errorf("lint flags the safe version: %s", worst)
				}
				if strings.Contains(name, "lock-queue") {
					st := run.Statements[len(run.Statements)-1]
					reads := run.BlockedTotal()["orders"].Reads
					if variant == "unsafe" && (st.LockWait < 5*time.Second || reads < 5*time.Second) {
						t.Errorf("expected a ~7s wait with reads queued: wait %s, reads blocked %s", st.LockWait, reads)
					}
					if variant == "safe" && (st.ErrorCode != "55P03" || reads > time.Second) {
						t.Errorf("expected lock_timeout to cut the wait: code %q, reads blocked %s", st.ErrorCode, reads)
					}
				}
			})
		}
	}
}
