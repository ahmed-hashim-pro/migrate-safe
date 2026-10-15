package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/measure"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/report"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/sqlparse"
)

const demoDB = "migrate_safe_demo"

func cmdSeed(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlags("seed", stderr, "--dsn URL [--rows N]")
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL URL of any database on the server (default $DATABASE_URL)")
	rows := fs.Int64("rows", 2_000_000, "orders rows to create")
	db := fs.String("db", demoDB, "name of the demo database to create")
	dir := fs.String("dir", "examples", "directory holding schema.sql")
	fresh := fs.Bool("fresh", false, "recreate the database even if it already exists with the same rows")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("--dsn is required (or set DATABASE_URL)")
	}
	url, err := seed(ctx, *dsn, *db, *rows, *dir, *fresh, func(f string, a ...any) { fmt.Fprintf(stderr, "  "+f+"\n", a...) })
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "demo database ready: %s\n", url)
	return nil
}

// seed creates the demo database with rows orders, unless it already exists
// with the same row count and schema file. It returns the database's URL.
func seed(ctx context.Context, dsn, db string, rows int64, dir string, fresh bool, log func(string, ...any)) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse --dsn: %w", err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "schema.sql"))
	if err != nil {
		return "", err
	}
	mark := fmt.Sprintf("migrate-safe demo rows=%d schema=%x", rows, sha256.Sum256(src))
	target := cfg.Copy()
	target.Database = db
	url, err := withDatabase(dsn, db)
	if err != nil {
		return "", err
	}

	adminCfg := cfg.Copy()
	adminCfg.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return "", err
	}
	defer admin.Close(context.Background())
	var comment *string
	err = admin.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, db).Scan(&comment)
	if err == nil && comment != nil && *comment == mark && !fresh {
		log("reusing %s (%s rows)", db, strconv.FormatInt(rows, 10))
		return url, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err == nil && (comment == nil || !strings.HasPrefix(*comment, "migrate-safe demo")) {
		return "", fmt.Errorf("database %s exists and was not made by migrate-safe; choose another name with --db", db)
	}
	log("creating %s with %d orders", db, rows)
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
		return "", err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		return "", err
	}
	conn, err := pgx.ConnectConfig(ctx, target)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	stmts, err := sqlparse.Split(strings.ReplaceAll(string(src), ":rows", strconv.FormatInt(rows, 10)))
	if err != nil {
		return "", err
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s.SQL); err != nil {
			return "", fmt.Errorf("schema.sql line %d: %w", s.Line, err)
		}
	}
	log("VACUUM ANALYZE")
	if _, err := conn.Exec(ctx, "VACUUM ANALYZE"); err != nil {
		return "", err
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS '%s'", pgx.Identifier{db}.Sanitize(), mark)); err != nil {
		return "", err
	}
	return url, nil
}

// withDatabase returns dsn pointing at database db, for both URL and
// key=value connection strings.
func withDatabase(dsn, db string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := neturl.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + db
		return u.String(), nil
	}
	return dsn + " dbname=" + db, nil
}

type variantResult struct {
	Example  string                 `json:"example"`
	Variant  string                 `json:"variant"`
	Lint     string                 `json:"lint"`
	Run      *measure.Run           `json:"measurement"`
	Disagree []measure.Disagreement `json:"lint_disagreements"`
	Unknown  []int                  `json:"not_analysed_lines,omitempty"`
}

func cmdExamples(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlags("examples", stderr, "--dsn URL [flags]")
	var mf measureFlags
	mf.register(fs)
	rows := fs.Int64("rows", 2_000_000, "orders rows in the demo database")
	dir := fs.String("dir", "examples", "examples directory")
	only := fs.String("only", "", "run only examples whose name contains this")
	check := fs.Bool("check", false, "exit 1 unless every lint prediction matches its measurement")
	svg := fs.String("svg", "", "write a chart of the results to this file")
	jsonOut := fs.String("json", "", "write every measurement to this JSON file")
	verbose := fs.Bool("v", false, "print each measurement in full")
	if _, err := parseArgs(fs, args); err != nil {
		return 2, err
	}
	if mf.workload == "" {
		mf.workload = filepath.Join(*dir, "workload.sql")
	}
	cfg, err := mf.config(stderr)
	if err != nil {
		return 2, err
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	url, err := seed(ctx, mf.dsn, demoDB, *rows, *dir, false, logf)
	if err != nil {
		return 2, err
	}
	cfg.DSN = url
	entries, err := os.ReadDir(filepath.Join(*dir, "migrations"))
	if err != nil {
		return 2, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), *only) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var results []variantResult
	problems := 0
	for _, name := range names {
		for _, variant := range []string{"unsafe", "safe"} {
			path := filepath.Join(*dir, "migrations", name, variant+".sql")
			stmts, err := readStatements(path)
			if err != nil {
				return 2, err
			}
			fmt.Fprintf(stderr, "%s/%s\n", name, variant)
			run, err := measure.Measure(ctx, cfg, stmts)
			if err != nil {
				return 2, fmt.Errorf("%s: %w", path, err)
			}
			worst := lint.OK
			for _, r := range run.Lint {
				worst = max(worst, r.Severity())
			}
			dis, un := measure.Agree(run)
			results = append(results, variantResult{Example: name, Variant: variant, Lint: worst.String(), Run: run, Disagree: dis, Unknown: un})
			if *verbose {
				report.Measurement(stdout, path, run, false)
				fmt.Fprintln(stdout)
			}
			var why []string
			if run.Failed != "" {
				why = append(why, "migration failed: "+run.Failed)
			}
			for _, d := range dis {
				why = append(why, fmt.Sprintf("line %d: %s", d.Line, d.What))
			}
			for _, l := range un {
				why = append(why, fmt.Sprintf("line %d: not analysed by lint", l))
			}
			if variant == "unsafe" && worst < lint.Risk {
				why = append(why, "lint found nothing wrong with the unsafe version")
			}
			if variant == "safe" && worst >= lint.Risk {
				why = append(why, "lint flags the safe version as "+worst.String())
			}
			for _, w := range why {
				fmt.Fprintf(stderr, "  PROBLEM: %s\n", w)
			}
			problems += len(why)
		}
	}
	table(stdout, results)
	if *svg != "" {
		if err := os.WriteFile(*svg, []byte(chart(results, *rows)), 0o644); err != nil {
			return 2, err
		}
	}
	if *jsonOut != "" {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return 2, err
		}
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			return 2, err
		}
	}
	if problems > 0 {
		fmt.Fprintf(stdout, "\n%d problem(s): see above\n", problems)
		if *check {
			return 1, nil
		}
	} else {
		fmt.Fprintln(stdout, "\nlint agreed with every measured lock, rewrite, scan and lock-queue wait")
	}
	return 0, nil
}

func longest(run *measure.Run, classes ...string) time.Duration {
	var d time.Duration
	for _, c := range classes {
		d = max(d, run.Overall[c].Max)
	}
	return d
}

func table(w io.Writer, results []variantResult) {
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "example\tvariant\tlint\ttook\treads locked out\twrites locked out\tlongest query\trewrote\tlint agrees")
	prev := ""
	for _, r := range results {
		name := r.Example
		if name == prev {
			name = ""
		}
		prev = r.Example
		var rew []string
		for _, s := range r.Run.Statements {
			rew = append(rew, s.Rewritten...)
		}
		rw := "-"
		if len(rew) > 0 {
			rw = strings.Join(uniq(rew), ",")
		}
		agrees := "yes"
		if len(r.Disagree) > 0 || len(r.Unknown) > 0 {
			agrees = "NO"
		}
		took := report.Dur(r.Run.To - r.Run.From)
		if r.Run.Failed != "" {
			took += " (failed)"
		}
		for _, s := range r.Run.Statements {
			if s.Expected {
				took += " (timed out, as intended)"
			}
		}
		reads, writes := lockedOut(r.Run)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, r.Variant, r.Lint, took,
			report.Dur(reads), report.Dur(writes), report.Dur(longest(r.Run, r.Run.Classes...)), rw, agrees)
	}
	tw.Flush()
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// lockedOut is how long the lock probes found reads and writes blocked, on
// whichever table was blocked longest. Unlike query latency, it counts only
// waits for table locks, so a busy disk does not inflate it.
func lockedOut(run *measure.Run) (reads, writes time.Duration) {
	for _, b := range run.BlockedTotal() {
		reads, writes = max(reads, b.Reads), max(writes, b.Writes)
	}
	return reads, writes
}

func stalled(run *measure.Run) (time.Duration, string) {
	reads, writes := lockedOut(run)
	all := max(reads, writes)
	r := reads >= all/2 && all >= 50*time.Millisecond
	w := writes >= all/2 && all >= 50*time.Millisecond
	switch {
	case r && w:
		return all, "reads and writes"
	case w:
		return all, "writes"
	case r:
		return all, "reads"
	}
	return all, ""
}

func chart(results []variantResult, rows int64) string {
	var pairs []report.Pair
	index := map[string]int{}
	for _, r := range results {
		i, ok := index[r.Example]
		if !ok {
			pairs = append(pairs, report.Pair{Name: strings.TrimLeft(r.Example, "0123456789-")})
			i = len(pairs) - 1
			index[r.Example] = i
		}
		d, what := stalled(r.Run)
		if r.Variant == "unsafe" {
			pairs[i].Unsafe, pairs[i].UnsafeWhat = d, what
		} else {
			pairs[i].Safe, pairs[i].SafeWhat = d, what
		}
	}
	server := ""
	if len(results) > 0 {
		server, _, _ = strings.Cut(results[0].Run.Server, " ")
	}
	return report.Chart(
		"How long the migration locked out reads or writes on a table",
		fmt.Sprintf("measured by lock probes every 20 ms · orders: %s rows · PostgreSQL %s · 4 readers, 4 updaters, 2 inserters running", commas(rows), server),
		pairs)
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
