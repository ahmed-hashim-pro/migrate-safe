// Package measure runs a migration against a real PostgreSQL server while
// application-like traffic runs, and records what actually happened: the
// locks each statement held, whether it rewrote or scanned a table, how long
// it waited in the lock queue, and how long the traffic was stuck.
package measure

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/sqlparse"
)

// Config controls a measurement run.
type Config struct {
	DSN string
	// InPlace runs the migration on the DSN's database itself. Otherwise the
	// database is copied (CREATE DATABASE ... TEMPLATE) and the copy is used.
	InPlace  bool
	Keep     bool // keep the shadow copy afterwards
	Workload *Workload
	Warmup   time.Duration // traffic before the first statement, for the baseline
	Tail     time.Duration // traffic after the last statement
	Probe    time.Duration // lock-probe interval
	Sample   time.Duration // pg_locks sampling interval
	Log      func(format string, args ...any)
}

func (c *Config) defaults() {
	if c.Warmup == 0 {
		c.Warmup = 2 * time.Second
	}
	if c.Tail == 0 {
		c.Tail = time.Second
	}
	if c.Probe == 0 {
		c.Probe = 20 * time.Millisecond
	}
	if c.Sample == 0 {
		c.Sample = 5 * time.Millisecond
	}
	if c.Log == nil {
		c.Log = func(string, ...any) {}
	}
}

// Run is everything one measurement observed.
type Run struct {
	Server   string                `json:"server"`
	PG       int                   `json:"pg_major"`
	Source   string                `json:"source_database"`
	Database string                `json:"database"`
	Rows     map[string]int64      `json:"rows"`
	Classes  []string              `json:"workload_classes"`
	Workers  map[string]int        `json:"workers"`
	Baseline map[string]ClassStats `json:"baseline"`
	// From and To bound the whole migration, first statement to last.
	From       time.Duration         `json:"from"`
	To         time.Duration         `json:"to"`
	Overall    map[string]ClassStats `json:"overall"`
	Statements []Stmt                `json:"statements"`
	Failed     string                `json:"failed,omitempty"`
	Lint       []lint.Result         `json:"-"`
	Probes     []ProbeSample         `json:"-"`
	Queries    []Query               `json:"-"`
	Tables     []string              `json:"tables"`
}

// Stmt is what one statement did.
type Stmt struct {
	Index int    `json:"index"`
	Line  int    `json:"line"`
	SQL   string `json:"sql"`
	Kind  string `json:"kind"`
	// From and To are offsets from the start of the run.
	From time.Duration `json:"from"`
	To   time.Duration `json:"to"`
	Took time.Duration `json:"took"`
	// Locks is the strongest table lock held, per table, while the statement
	// ran. Exact when LocksExact (read from pg_locks before the transaction
	// ended); otherwise sampled every few milliseconds.
	Locks      map[string]lint.Mode `json:"locks"`
	LocksExact bool                 `json:"locks_exact"`
	Rewritten  []string             `json:"rewritten"`
	// Scanned lists tables with at least one sequential scan by this
	// statement (pg_stat_xact_user_tables / pg_stat_user_tables).
	Scanned      []string      `json:"scanned"`
	ScanMeasured bool          `json:"scan_measured"`
	Rows         int64         `json:"rows_affected"`
	Batches      int           `json:"batches,omitempty"`
	LockWait     time.Duration `json:"lock_wait"`
	Error        string        `json:"error,omitempty"`
	ErrorCode    string        `json:"error_code,omitempty"`
	Expected     bool          `json:"error_expected,omitempty"`
	Skipped      bool          `json:"skipped,omitempty"`
	LongReader   *LongReader   `json:"long_reader,omitempty"`
	// Classes is the workload's latency during the statement, per class.
	Classes map[string]ClassStats `json:"workload"`
	// Blocked is how long the lock probes found reads and writes blocked
	// on each table during the statement.
	Blocked map[string]Blocked `json:"blocked"`
}

// LongReader describes a long-running transaction started before a
// statement, from a `-- migrate-safe: long-reader table duration` directive.
type LongReader struct {
	Table string        `json:"table"`
	From  time.Duration `json:"from"`
	Hold  time.Duration `json:"hold"`
}

// Blocked is probe-measured blocked time on one table.
type Blocked struct {
	Reads  time.Duration `json:"reads"`
	Writes time.Duration `json:"writes"`
}

// ClassStats summarises one workload class's queries in a window.
type ClassStats struct {
	Count  int           `json:"count"`
	Errors int           `json:"errors"`
	P50    time.Duration `json:"p50"`
	P99    time.Duration `json:"p99"`
	Max    time.Duration `json:"max"`
}

// Measure runs the migration and returns what it observed. A statement that
// fails ends the run unless it carries `-- migrate-safe: expect-error CODE`.
func Measure(ctx context.Context, cfg Config, stmts []sqlparse.Statement) (*Run, error) {
	cfg.defaults()
	pcfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse --dsn: %w", err)
	}
	run := &Run{Source: pcfg.Database, Database: pcfg.Database, Rows: map[string]int64{}, Workers: map[string]int{}}
	if run.Source == "" {
		return nil, errors.New("--dsn must name a database")
	}

	admin, err := connectTo(ctx, pcfg, "postgres")
	if err != nil {
		return nil, err
	}
	defer admin.Close(context.Background())
	var num int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version'), current_setting('server_version_num')::int").Scan(&run.Server, &num); err != nil {
		return nil, err
	}
	run.PG = num / 10000

	if !cfg.InPlace {
		shadow, err := makeShadow(ctx, admin, run.Source, run.PG)
		if err != nil {
			return nil, err
		}
		run.Database = shadow
		cfg.Log("copied %s to %s", run.Source, shadow)
		if !cfg.Keep {
			defer func() {
				c, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				drop := "DROP DATABASE IF EXISTS " + pgx.Identifier{shadow}.Sanitize()
				if run.PG >= 13 {
					drop += " WITH (FORCE)"
				}
				if _, err := admin.Exec(c, drop); err != nil {
					cfg.Log("could not drop %s: %v", shadow, err)
				}
			}()
		}
	}

	open := func() (*pgx.Conn, error) { return connectTo(ctx, pcfg, run.Database) }
	mig, err := open()
	if err != nil {
		return nil, err
	}
	defer mig.Close(context.Background())
	schema, err := LoadSchema(ctx, mig)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	run.Lint = lint.File(stmts, lint.Options{PG: run.PG, Schema: schema})
	for t := range schema.Tables {
		run.Tables = append(run.Tables, t)
	}
	sort.Strings(run.Tables)
	rows, err := mig.Query(ctx, `SELECT relname, greatest(reltuples, 0)::bigint FROM pg_class
		WHERE relkind IN ('r','p') AND relnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)
		  AND relnamespace::regnamespace::text NOT LIKE 'pg_toast%'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t string
		var n int64
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		run.Rows[t] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pid := mig.PgConn().PID()

	origin := time.Now()
	obsCtx, stopObs := context.WithCancel(ctx)
	defer stopObs()
	var obsWG sync.WaitGroup
	obsErr := make(chan error, 2)
	var current atomic.Int64
	current.Store(-1)

	probeConn, err := open()
	if err != nil {
		return nil, err
	}
	defer probeConn.Close(context.Background())
	pr := &prober{conn: probeConn, tables: run.Tables, tick: cfg.Probe, origin: origin}
	sampConn, err := open()
	if err != nil {
		return nil, err
	}
	defer sampConn.Close(context.Background())
	sm := &sampler{conn: sampConn, pid: pid, tick: cfg.Sample, origin: origin, current: func() int { return int(current.Load()) }}
	for _, f := range []func(context.Context) error{pr.run, sm.run} {
		obsWG.Add(1)
		go func(f func(context.Context) error) {
			defer obsWG.Done()
			if err := f(obsCtx); err != nil {
				obsErr <- err
			}
		}(f)
	}

	rec := &recorder{}
	wlCtx, stopWL := context.WithCancel(ctx)
	defer stopWL()
	var wlWG sync.WaitGroup
	if w := cfg.Workload; w != nil && w.Workers() > 0 {
		pc, err := pgxpool.ParseConfig(cfg.DSN)
		if err != nil {
			return nil, err
		}
		pc.ConnConfig.Database = run.Database
		pc.MaxConns = int32(w.Workers() + 1)
		pool, err := pgxpool.NewWithConfig(ctx, pc)
		if err != nil {
			return nil, err
		}
		defer pool.Close()
		if err := w.resolve(ctx, mig); err != nil {
			return nil, err
		}
		for _, c := range w.Classes {
			run.Classes = append(run.Classes, c.Name)
			run.Workers[c.Name] = c.Workers
		}
		wlWG.Add(1)
		go func() { defer wlWG.Done(); w.run(wlCtx, pool, origin, rec) }()
		cfg.Log("workload running: %d workers", w.Workers())
	}
	sleep(ctx, cfg.Warmup)
	warm := time.Since(origin)

	x := &executor{ctx: ctx, conn: mig, pg: run.PG, origin: origin, log: cfg.Log}
	var readers sync.WaitGroup
	skipTx := false
	for i, r := range run.Lint {
		current.Store(int64(i))
		st := Stmt{Index: i, Line: r.Stmt.Line, SQL: r.Stmt.SQL, Kind: r.Kind}
		if skipTx {
			st.Skipped = true
			if r.TxControl && r.Kind == "COMMIT" {
				skipTx = false
			}
			run.Statements = append(run.Statements, st)
			continue
		}
		if v, ok := r.Stmt.Directive("long-reader"); ok {
			lr, err := startLongReader(ctx, open, v, origin, &readers)
			if err != nil {
				return nil, err
			}
			st.LongReader = lr
			cfg.Log("line %d: a transaction now holds ACCESS SHARE on %s for %s", r.Stmt.Line, lr.Table, lr.Hold)
			sleep(ctx, time.Second)
		}
		cfg.Log("line %d: %s", r.Stmt.Line, oneLine(r.Stmt.SQL, 70))
		err := x.exec(r, &st)
		st.Took = st.To - st.From
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) {
				st.ErrorCode = pe.Code
			}
			st.Error = err.Error()
			want, _ := r.Stmt.Directive("expect-error")
			if want != "" && want == st.ErrorCode {
				st.Expected = true
				cfg.Log("line %d: failed as expected (%s)", r.Stmt.Line, st.ErrorCode)
				if r.InTx && !r.TxControl {
					skipTx = true
				}
			} else {
				run.Failed = fmt.Sprintf("line %d: %v", r.Stmt.Line, err)
				run.Statements = append(run.Statements, st)
				break
			}
		}
		run.Statements = append(run.Statements, st)
	}
	current.Store(-1)
	run.From, run.To = warm, warm
	if len(run.Statements) > 0 {
		run.From = run.Statements[0].From
		run.To = run.Statements[len(run.Statements)-1].To
	}
	readers.Wait()
	sleep(ctx, cfg.Tail)
	stopWL()
	wlWG.Wait()
	stopObs()
	obsWG.Wait()
	select {
	case err := <-obsErr:
		return nil, err
	default:
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	run.Probes = pr.samples
	run.Queries = rec.snapshot()
	run.Baseline = classStats(run.Queries, run.Classes, 0, warm, true)
	run.Overall = classStats(run.Queries, run.Classes, run.From, run.To, false)
	for i := range run.Statements {
		st := &run.Statements[i]
		if st.Skipped {
			continue
		}
		st.Classes = classStats(run.Queries, run.Classes, st.From, st.To, false)
		st.Blocked = map[string]Blocked{}
		for _, t := range run.Tables {
			st.Blocked[t] = Blocked{
				Reads:  blockedIn(pr.samples, t, false, st.From, st.To),
				Writes: blockedIn(pr.samples, t, true, st.From, st.To),
			}
		}
		var waited time.Duration
		var prev *LockSample
		for k := range sm.samples {
			s := &sm.samples[k]
			if s.Stmt != st.Index {
				prev = nil
				continue
			}
			if prev != nil && prev.Waiting {
				waited += s.At - prev.At
			}
			prev = s
			if !st.LocksExact {
				for t, m := range s.Granted {
					if m > st.Locks[t] {
						st.Locks[t] = m
					}
				}
			}
		}
		st.LockWait = waited
	}
	return run, nil
}

// BlockedTotal is the probe-measured blocked time for the whole migration.
func (r *Run) BlockedTotal() map[string]Blocked {
	out := map[string]Blocked{}
	for _, t := range r.Tables {
		out[t] = Blocked{Reads: blockedIn(r.Probes, t, false, r.From, r.To), Writes: blockedIn(r.Probes, t, true, r.From, r.To)}
	}
	return out
}

func connectTo(ctx context.Context, base *pgx.ConnConfig, db string) (*pgx.Conn, error) {
	c := base.Copy()
	c.Database = db
	conn, err := pgx.ConnectConfig(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", db, err)
	}
	return conn, nil
}

func makeShadow(ctx context.Context, admin *pgx.Conn, src string, pg int) (string, error) {
	var others int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = $1", src).Scan(&others); err != nil {
		return "", err
	}
	if others > 0 {
		return "", fmt.Errorf("copying %s needs it to have no other sessions, and it has %d: point --dsn at a restored copy of production, or use --in-place on a database you can throw away", src, others)
	}
	name := fmt.Sprintf("%s_shadow_%d", src, time.Now().UnixNano()%1_000_000)
	if len(name) > 63 {
		name = name[len(name)-63:]
	}
	q := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{src}.Sanitize())
	if pg >= 15 {
		q += " STRATEGY FILE_COPY"
	}
	if _, err := admin.Exec(ctx, q); err != nil {
		return "", fmt.Errorf("copy %s: %w", src, err)
	}
	return name, nil
}

var longReaderRe = regexp.MustCompile(`^(\S+)\s+(\S+)$`)

func startLongReader(ctx context.Context, open func() (*pgx.Conn, error), v string, origin time.Time, wg *sync.WaitGroup) (*LongReader, error) {
	m := longReaderRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return nil, fmt.Errorf("long-reader wants `table duration`, got %q", v)
	}
	hold, err := time.ParseDuration(m[2])
	if err != nil {
		return nil, fmt.Errorf("long-reader: %w", err)
	}
	conn, err := open()
	if err != nil {
		return nil, err
	}
	lr := &LongReader{Table: m[1], From: time.Since(origin), Hold: hold}
	if _, err := conn.Exec(ctx, "BEGIN; LOCK TABLE "+pgx.Identifier{m[1]}.Sanitize()+" IN ACCESS SHARE MODE"); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("long-reader: %w", err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer conn.Close(context.Background())
		sleep(ctx, hold-time.Since(origin)+lr.From)
		conn.Exec(ctx, "COMMIT")
	}()
	return lr, nil
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func classStats(qs []Query, classes []string, from, to time.Duration, within bool) map[string]ClassStats {
	lat := make([][]time.Duration, len(classes))
	errs := make([]int, len(classes))
	for _, q := range qs {
		in := q.Start < to && q.End > from
		if within {
			in = q.Start >= from && q.End <= to
		}
		if !in {
			continue
		}
		lat[q.Class] = append(lat[q.Class], q.End-q.Start)
		if q.Err {
			errs[q.Class]++
		}
	}
	out := map[string]ClassStats{}
	for i, c := range classes {
		l := lat[i]
		sort.Slice(l, func(a, b int) bool { return l[a] < l[b] })
		cs := ClassStats{Count: len(l), Errors: errs[i]}
		if len(l) > 0 {
			cs.P50 = l[len(l)/2]
			cs.P99 = l[min(len(l)-1, len(l)*99/100)]
			cs.Max = l[len(l)-1]
		}
		out[c] = cs
	}
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

// executor runs statements on the migration connection and measures each.
type executor struct {
	ctx    context.Context
	conn   *pgx.Conn
	pg     int
	origin time.Time
	log    func(string, ...any)
	inTx   bool
}

func (x *executor) now() time.Duration { return time.Since(x.origin) }

type snapshot struct {
	files map[string]uint32
	scans map[string]int64
}

func (x *executor) snap(xact bool) (snapshot, error) {
	s := snapshot{files: map[string]uint32{}, scans: map[string]int64{}}
	rows, err := x.conn.Query(x.ctx, `SELECT relname, relfilenode::bigint FROM pg_class
		WHERE relkind IN ('r','p') AND relnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)
		  AND relnamespace::regnamespace::text NOT LIKE 'pg_toast%'`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var t string
		var f int64
		if err := rows.Scan(&t, &f); err != nil {
			return s, err
		}
		s.files[t] = uint32(f)
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	view := "pg_stat_xact_user_tables"
	if !xact {
		view = "pg_stat_user_tables"
	}
	rows, err = x.conn.Query(x.ctx, "SELECT relname, coalesce(seq_scan, 0) FROM "+view)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var t string
		var n int64
		if err := rows.Scan(&t, &n); err != nil {
			return s, err
		}
		s.scans[t] = n
	}
	return s, rows.Err()
}

func (x *executor) myLocks() (map[string]lint.Mode, error) {
	out := map[string]lint.Mode{}
	rows, err := x.conn.Query(x.ctx, lockQuery, x.conn.PgConn().PID())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel, mode string
		var granted bool
		if err := rows.Scan(&rel, &mode, &granted); err != nil {
			return nil, err
		}
		if m, ok := lint.ParsePGLock(mode); ok && granted && m > out[rel] {
			out[rel] = m
		}
	}
	return out, rows.Err()
}

// diff records rewrites and scans between two snapshots.
func diff(st *Stmt, before, after snapshot) {
	for t, f := range after.files {
		if b, ok := before.files[t]; ok && b != f && !contains(st.Rewritten, t) {
			st.Rewritten = append(st.Rewritten, t)
		}
	}
	for t, n := range after.scans {
		if n > before.scans[t] && !contains(st.Scanned, t) {
			st.Scanned = append(st.Scanned, t)
		}
	}
	sort.Strings(st.Rewritten)
	sort.Strings(st.Scanned)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (x *executor) exec(r lint.Result, st *Stmt) error {
	st.Locks = map[string]lint.Mode{}
	switch {
	case r.TxControl:
		st.From = x.now()
		_, err := x.conn.Exec(x.ctx, r.Stmt.SQL)
		st.To = x.now()
		x.inTx = r.Kind == "BEGIN" && err == nil
		return err
	case x.inTx:
		return x.inTransaction(r.Stmt.SQL, st)
	case r.Batch != nil:
		return x.batched(r, st)
	case r.NoTx:
		return x.outsideTransaction(r.Stmt.SQL, st)
	default:
		return x.wrapped(r.Stmt.SQL, st, true)
	}
}

// inTransaction runs a statement inside the migration's own BEGIN.
func (x *executor) inTransaction(sql string, st *Stmt) error {
	before, err := x.snap(true)
	if err != nil {
		return err
	}
	st.From = x.now()
	tag, err := x.conn.Exec(x.ctx, sql)
	st.To = x.now()
	if err != nil {
		x.conn.Exec(x.ctx, "ROLLBACK")
		x.inTx = false
		return err
	}
	st.Rows = tag.RowsAffected()
	after, err := x.snap(true)
	if err != nil {
		return err
	}
	diff(st, before, after)
	st.ScanMeasured = true
	if st.Locks, err = x.myLocks(); err != nil {
		return err
	}
	st.LocksExact = true
	return nil
}

// wrapped runs one statement in its own transaction so that its locks can
// be read from pg_locks before they are released. Wrapping changes nothing
// about how the statement locks: a lone statement holds its locks until it
// commits either way.
func (x *executor) wrapped(sql string, st *Stmt, timed bool) error {
	if _, err := x.conn.Exec(x.ctx, "BEGIN"); err != nil {
		return err
	}
	before, err := x.snap(true)
	if err != nil {
		return err
	}
	if timed {
		st.From = x.now()
	}
	tag, err := x.conn.Exec(x.ctx, sql)
	if err != nil {
		st.To = x.now()
		x.conn.Exec(x.ctx, "ROLLBACK")
		return err
	}
	st.Rows += tag.RowsAffected()
	after, err := x.snap(true)
	if err != nil {
		return err
	}
	diff(st, before, after)
	st.ScanMeasured = true
	locks, err := x.myLocks()
	if err != nil {
		return err
	}
	for t, m := range locks {
		if m > st.Locks[t] {
			st.Locks[t] = m
		}
	}
	st.LocksExact = true
	_, err = x.conn.Exec(x.ctx, "COMMIT")
	st.To = x.now()
	return err
}

// outsideTransaction runs a statement that refuses to run in a transaction
// block (CREATE INDEX CONCURRENTLY, VACUUM). Its locks come from the sampler.
func (x *executor) outsideTransaction(sql string, st *Stmt) error {
	stats := x.pg >= 15 // pg_stat_force_next_flush
	var before snapshot
	var err error
	if stats {
		if before, err = x.flushedSnap(); err != nil {
			return err
		}
	}
	st.From = x.now()
	tag, err := x.conn.Exec(x.ctx, sql)
	st.To = x.now()
	if err != nil {
		return err
	}
	st.Rows = tag.RowsAffected()
	if stats {
		after, err := x.flushedSnap()
		if err != nil {
			return err
		}
		diff(st, before, after)
		st.ScanMeasured = true
	}
	return nil
}

// flushedSnap reads shared statistics after this backend has flushed its own.
func (x *executor) flushedSnap() (snapshot, error) {
	if _, err := x.conn.Exec(x.ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
		return snapshot{}, err
	}
	if _, err := x.conn.Exec(x.ctx, "SELECT 1"); err != nil { // ends a transaction: the flush happens
		return snapshot{}, err
	}
	if _, err := x.conn.Exec(x.ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
		return snapshot{}, err
	}
	return x.snap(false)
}

var rangeRe = regexp.MustCompile(`(^|[^:]):(lo|hi)\b`)

// batched runs a statement once per [:lo, :hi) range, each in its own
// transaction.
func (x *executor) batched(r lint.Result, st *Stmt) error {
	b := r.Batch
	var lo, hi int64
	q := fmt.Sprintf("SELECT coalesce(min(%[1]s),0)::bigint, coalesce(max(%[1]s),0)::bigint FROM %[2]s",
		pgx.Identifier{b.Column}.Sanitize(), pgx.Identifier{b.Table}.Sanitize())
	if err := x.conn.QueryRow(x.ctx, q).Scan(&lo, &hi); err != nil {
		return err
	}
	st.From = x.now()
	for start := lo; start <= hi; start += b.Size {
		sql := rangeRe.ReplaceAllStringFunc(r.Stmt.SQL, func(m string) string {
			lead := ""
			if m[0] != ':' {
				lead, m = m[:1], m[1:]
			}
			if m == ":lo" {
				return lead + strconv.FormatInt(start, 10)
			}
			return lead + strconv.FormatInt(start+b.Size, 10)
		})
		if err := x.wrapped(sql, st, false); err != nil {
			return err
		}
		st.Batches++
	}
	st.To = x.now()
	return nil
}
