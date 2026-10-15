package measure

import (
	"fmt"
	"sort"
	"time"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
)

// Disagreement is a place where the linter's prediction and the measurement
// differ.
type Disagreement struct {
	Line int    `json:"line"`
	What string `json:"what"`
}

// Agree compares every statement's lint prediction with what was measured:
// the lock on each table, which tables were rewritten, which were scanned,
// whether DML touched every row, and, for statements run behind a long
// transaction, whether the statement waited and stalled traffic.
// Statements the linter could not analyse are returned separately.
func Agree(run *Run) (out []Disagreement, unanalysed []int) {
	for i, st := range run.Statements {
		if st.Skipped || i >= len(run.Lint) {
			continue
		}
		r := run.Lint[i]
		if r.TxControl {
			continue
		}
		if r.Unknown != "" {
			unanalysed = append(unanalysed, r.Stmt.Line)
			continue
		}
		bad := func(format string, args ...any) {
			out = append(out, Disagreement{Line: r.Stmt.Line, What: fmt.Sprintf(format, args...)})
		}
		if st.LongReader != nil {
			queue(run, r, st, bad)
		}
		if st.Error != "" {
			continue // rolled back: nothing structural to compare
		}
		tables := map[string]bool{}
		for t := range r.Held {
			tables[t] = true
		}
		for t, m := range st.Locks {
			if m > lint.AccessShare {
				tables[t] = true
			}
		}
		// A statement outside a transaction that finished between two samples
		// left no trace in pg_locks: its locks can't be compared.
		locksSeen := st.LocksExact || st.LockSamples > 0
		for _, t := range sorted(tables) {
			if r.NewTables[t] || !locksSeen {
				continue
			}
			want, got := r.Held[t], st.Locks[t]
			if want == got || want == lint.None && got == lint.AccessShare {
				continue
			}
			how := "pg_locks"
			if !st.LocksExact {
				how = "sampled pg_locks"
			}
			bad("lock on %s: lint predicted %s, %s showed %s", t, want, how, got)
		}
		for _, e := range r.Effects {
			if r.NewTables[e.Table] {
				continue
			}
			if got := contains(st.Rewritten, e.Table); got != e.Rewrite {
				bad("rewrite of %s: lint predicted %v, relfilenode says %v", e.Table, e.Rewrite, got)
			}
			if e.ScanKnown && st.ScanMeasured {
				if got := contains(st.Scanned, e.Table); got != e.Scan {
					bad("full scan of %s: lint predicted %v, seq_scan says %v", e.Table, e.Scan, got)
				}
			}
			if e.AllRowsKnown && r.Batch == nil && (e.Lock == lint.RowExclusive) {
				n := run.Rows[e.Table]
				got := n > 0 && st.Rows*10 >= n*9
				if got != e.AllRows {
					bad("rows locked in %s: lint predicted all=%v, the statement touched %d of %d", e.Table, e.AllRows, st.Rows, n)
				}
			}
		}
		for _, t := range st.Rewritten {
			if !hasEffect(r, t) && !r.NewTables[t] {
				bad("%s was rewritten, which lint did not predict", t)
			}
		}
	}
	return out, unanalysed
}

// queue checks a statement that ran while a long transaction held ACCESS
// SHARE on a table: it should wait if its lock conflicts and there is no
// lock_timeout, fail fast if there is one, and not wait otherwise.
func queue(run *Run, r lint.Result, st Stmt, bad func(string, ...any)) {
	lr := st.LongReader
	mode := lint.None
	for _, e := range r.Effects {
		if e.Table == lr.Table {
			mode = e.Lock
		}
	}
	remaining := lr.From + lr.Hold - st.From
	conflicts := lint.Conflicts(mode, lint.AccessShare)
	switch {
	case !conflicts:
		if st.LockWait > remaining/2 {
			bad("lint predicted no wait behind the open transaction on %s, but the statement waited %s", lr.Table, st.LockWait.Round(time.Millisecond))
		}
	case r.LockTimeout > 0 && r.LockTimeout < remaining:
		if st.ErrorCode != "55P03" {
			bad("lint predicted lock_timeout (%s) to cancel the wait on %s, but the statement ended with %q", r.LockTimeout, lr.Table, st.ErrorCode)
		}
		if st.Took > r.LockTimeout+time.Second {
			bad("lint predicted the wait on %s to end after %s, but it took %s", lr.Table, r.LockTimeout, st.Took.Round(time.Millisecond))
		}
	default:
		if st.LockWait < remaining/2 {
			bad("lint predicted a wait of about %s behind the open transaction on %s, but the statement waited %s", remaining.Round(100*time.Millisecond), lr.Table, st.LockWait.Round(time.Millisecond))
		}
		b := blockedIn(run.Probes, lr.Table, false, st.From, st.To)
		if mode.BlocksReads() && b < st.LockWait/2 {
			bad("lint predicted reads on %s to queue behind the waiting %s, but probes found them blocked only %s", lr.Table, mode, b.Round(time.Millisecond))
		}
	}
}

func hasEffect(r lint.Result, t string) bool {
	for _, e := range r.Effects {
		if e.Table == t {
			return true
		}
	}
	return false
}

func sorted(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
