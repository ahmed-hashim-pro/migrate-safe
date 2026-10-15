// Command migrate-safe finds out what a PostgreSQL migration will do to a
// live database before it ships: statically, with `lint`, and for real, with
// `measure`, which runs it on a copy of the database under replayed traffic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/measure"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/report"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/sqlparse"
)

var version = "dev"

const usage = `migrate-safe: what will this PostgreSQL migration do to a live database?

usage:
  migrate-safe lint [flags] migration.sql...
      Predict, without a database, the lock each statement takes, whether it
      rewrites or scans a table while holding it, and whether it can wait in
      the lock queue. Exits 1 on any DANGER or RISK.

  migrate-safe measure --dsn URL [flags] migration.sql
      Copy the database, replay traffic against the copy, run the migration
      on it, and report the locks, rewrites, scans and stalls it caused.

  migrate-safe seed --dsn URL [--rows N]
      Create the demo database (orders and accounts) to measure against.

  migrate-safe examples --dsn URL [flags]
      Measure every example migration as written and as rewritten, and check
      the linter against each measurement.

Run "migrate-safe <command> -h" for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	code := 0
	switch args[0] {
	case "lint":
		code, err = cmdLint(args[1:], stdout, stderr)
	case "measure":
		code, err = cmdMeasure(ctx, args[1:], stdout, stderr)
	case "seed":
		err = cmdSeed(ctx, args[1:], stdout, stderr)
	case "examples":
		code, err = cmdExamples(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "migrate-safe", version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "migrate-safe:", err)
		if code == 0 {
			code = 2
		}
	}
	return code
}

func newFlags(name string, stderr io.Writer, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: migrate-safe %s %s\n\nflags:\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs lets flags come after the file names, as people type them.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var files []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return files, nil
		}
		files = append(files, args[0])
		args = args[1:]
	}
}

func readStatements(path string) ([]sqlparse.Statement, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stmts, err := sqlparse.Split(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return stmts, nil
}

func cmdLint(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlags("lint", stderr, "[flags] migration.sql...")
	pg := fs.Int("pg", lint.DefaultPG, "PostgreSQL major version the migration will run on")
	schemaPath := fs.String("schema", "", "SQL file describing the current schema (CREATE TABLE ...), for column types and constraints")
	failOn := fs.String("fail-on", "risk", "exit 1 on findings at this level or worse: danger, risk or never")
	verbose := fs.Bool("v", false, "show every statement, with notes")
	asJSON := fs.Bool("json", false, "print JSON")
	files, err := parseArgs(fs, args)
	if err != nil {
		return 2, err
	}
	if len(files) == 0 {
		fs.Usage()
		return 2, errors.New("no migration files given")
	}
	threshold := map[string]lint.Severity{"danger": lint.Danger, "risk": lint.Risk, "never": lint.Danger + 1}[*failOn]
	if threshold == 0 {
		return 2, fmt.Errorf("--fail-on must be danger, risk or never")
	}
	schema := lint.NewSchema()
	if *schemaPath != "" {
		stmts, err := readStatements(*schemaPath)
		if err != nil {
			return 2, err
		}
		schema = lint.ApplySchema(stmts, *pg)
	}
	worst := lint.OK
	var all []jsonLint
	for i, f := range files {
		stmts, err := readStatements(f)
		if err != nil {
			return 2, err
		}
		res := lint.File(stmts, lint.Options{PG: *pg, Schema: schema})
		if *asJSON {
			for _, r := range res {
				all = append(all, toJSON(f, r))
				worst = max(worst, r.Severity())
			}
			continue
		}
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		worst = max(worst, report.Lint(stdout, f, *pg, res, *verbose))
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(all); err != nil {
			return 2, err
		}
	}
	if worst >= threshold {
		return 1, nil
	}
	return 0, nil
}

type jsonEffect struct {
	Table   string `json:"table"`
	Lock    string `json:"lock"`
	Rewrite bool   `json:"rewrite"`
	Scan    *bool  `json:"scan"`
	AllRows *bool  `json:"all_rows"`
}

type jsonFinding struct {
	Severity string `json:"severity"`
	Table    string `json:"table,omitempty"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

type jsonLint struct {
	File        string            `json:"file"`
	Line        int               `json:"line"`
	SQL         string            `json:"sql"`
	Kind        string            `json:"kind"`
	Effects     []jsonEffect      `json:"effects"`
	Held        map[string]string `json:"held"`
	LockTimeout string            `json:"lock_timeout,omitempty"`
	NoTx        bool              `json:"no_transaction,omitempty"`
	Findings    []jsonFinding     `json:"findings"`
	Unknown     string            `json:"not_analysed,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
}

func toJSON(file string, r lint.Result) jsonLint {
	j := jsonLint{File: file, Line: r.Stmt.Line, SQL: r.Stmt.SQL, Kind: r.Kind, Held: map[string]string{},
		NoTx: r.NoTx, Unknown: r.Unknown, Notes: r.Notes, Effects: []jsonEffect{}, Findings: []jsonFinding{}}
	if r.LockTimeout > 0 {
		j.LockTimeout = r.LockTimeout.String()
	}
	for t, m := range r.Held {
		j.Held[t] = m.String()
	}
	for _, e := range r.Effects {
		je := jsonEffect{Table: e.Table, Lock: e.Lock.String(), Rewrite: e.Rewrite}
		if e.ScanKnown {
			v := e.Scan
			je.Scan = &v
		}
		if e.AllRowsKnown {
			v := e.AllRows
			je.AllRows = &v
		}
		j.Effects = append(j.Effects, je)
	}
	for _, f := range r.Findings {
		j.Findings = append(j.Findings, jsonFinding{Severity: strings.ToLower(f.Severity.String()), Table: f.Table, Message: f.Message, Fix: f.Fix})
	}
	return j
}

type measureFlags struct {
	dsn, workload, workers string
	inPlace, keep, quiet   bool
}

func (m *measureFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&m.dsn, "dsn", os.Getenv("DATABASE_URL"), "PostgreSQL URL of the database to copy (default $DATABASE_URL)")
	fs.StringVar(&m.workload, "workload", "", "workload file to replay during the migration (default: examples/workload.sql if it exists; \"none\" for no traffic)")
	fs.StringVar(&m.workers, "workers", "", "override worker counts, e.g. read=8,write=2")
	fs.BoolVar(&m.inPlace, "in-place", false, "run on the database itself instead of a copy (it will be changed)")
	fs.BoolVar(&m.keep, "keep", false, "keep the copy afterwards")
	fs.BoolVar(&m.quiet, "q", false, "no progress output")
}

func (m *measureFlags) config(stderr io.Writer) (measure.Config, error) {
	cfg := measure.Config{DSN: m.dsn, InPlace: m.inPlace, Keep: m.keep}
	if m.dsn == "" {
		return cfg, errors.New("--dsn is required (or set DATABASE_URL)")
	}
	if !m.quiet {
		cfg.Log = func(format string, a ...any) { fmt.Fprintf(stderr, "  "+format+"\n", a...) }
	}
	path := m.workload
	if path == "" {
		if _, err := os.Stat(filepath.Join("examples", "workload.sql")); err == nil {
			path = filepath.Join("examples", "workload.sql")
		}
	}
	if path != "" && path != "none" {
		src, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		w, err := measure.ParseWorkload(string(src))
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
		if err := w.SetWorkers(m.workers); err != nil {
			return cfg, err
		}
		cfg.Workload = w
	}
	return cfg, nil
}

func cmdMeasure(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlags("measure", stderr, "--dsn URL [flags] migration.sql")
	var mf measureFlags
	mf.register(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	verbose := fs.Bool("v", false, "also show BEGIN/COMMIT")
	files, err := parseArgs(fs, args)
	if err != nil {
		return 2, err
	}
	if len(files) != 1 {
		fs.Usage()
		return 2, errors.New("give exactly one migration file")
	}
	cfg, err := mf.config(stderr)
	if err != nil {
		return 2, err
	}
	stmts, err := readStatements(files[0])
	if err != nil {
		return 2, err
	}
	run, err := measure.Measure(ctx, cfg, stmts)
	if err != nil {
		return 2, err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		dis, _ := measure.Agree(run)
		return boolCode(run.Failed != ""), enc.Encode(struct {
			*measure.Run
			Disagreements []measure.Disagreement `json:"lint_disagreements"`
		}{run, dis})
	}
	report.Measurement(stdout, files[0], run, *verbose)
	return boolCode(run.Failed != ""), nil
}

func boolCode(b bool) int {
	if b {
		return 1
	}
	return 0
}
