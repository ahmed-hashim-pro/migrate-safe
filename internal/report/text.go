// Package report renders lint results and measurements for people.
package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
	"github.com/ahmed-hashim-pro/migrate-safe/internal/measure"
)

// Dur formats a duration for a table: "0.4 ms", "12 ms", "4.12 s".
func Dur(d time.Duration) string {
	switch {
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n-1] + "…"
	}
	return s
}

// effects summarises what a statement does: "SHARE on orders · reads the whole table".
func effects(r lint.Result) string {
	var parts []string
	for _, e := range r.Effects {
		p := fmt.Sprintf("%s on %s", e.Lock, e.Table)
		if held := r.Held[e.Table]; held > e.Lock {
			p += fmt.Sprintf(" (holding %s)", held)
		}
		switch {
		case e.Rewrite:
			p += " · rewrites it"
		case e.Scan:
			p += " · reads every row"
		}
		if e.AllRows && e.AllRowsKnown {
			p += " · locks every row"
		}
		parts = append(parts, p)
	}
	if r.NoTx {
		parts = append(parts, "outside a transaction")
	}
	if r.LockTimeout > 0 {
		parts = append(parts, "lock_timeout "+r.LockTimeout.String())
	}
	if len(parts) == 0 {
		return "no table locks"
	}
	return strings.Join(parts, " · ")
}

// Lint writes the linter's findings. It returns the worst severity seen.
func Lint(w io.Writer, name string, pg int, results []lint.Result, verbose bool) lint.Severity {
	fmt.Fprintf(w, "%s (PostgreSQL %d)\n", name, pg)
	worst := lint.OK
	counts := map[lint.Severity]int{}
	unknown := 0
	for _, r := range results {
		if r.TxControl {
			continue
		}
		sev := r.Severity()
		worst = max(worst, sev)
		if r.Unknown != "" {
			unknown++
		}
		if sev < lint.Risk && r.Unknown == "" && !verbose {
			continue
		}
		fmt.Fprintf(w, "\nline %-4d %s\n", r.Stmt.Line, oneLine(r.Stmt.SQL, 72))
		if r.Unknown != "" {
			fmt.Fprintf(w, "          not analysed: %s\n", r.Unknown)
			continue
		}
		fmt.Fprintf(w, "          %s\n", effects(r))
		for _, f := range r.Findings {
			counts[f.Severity]++
			if f.Severity < lint.Risk && !verbose {
				continue
			}
			fmt.Fprintf(w, "  %-7s %s\n", f.Severity, f.Message)
			if f.Fix != "" {
				fmt.Fprintf(w, "          fix: %s\n", f.Fix)
			}
		}
		if verbose {
			for _, n := range r.Notes {
				fmt.Fprintf(w, "          note: %s\n", n)
			}
		}
	}
	var sum []string
	if n := counts[lint.Danger]; n > 0 {
		sum = append(sum, plural(n, "danger"))
	}
	if n := counts[lint.Risk]; n > 0 {
		sum = append(sum, plural(n, "risk"))
	}
	if unknown > 0 {
		sum = append(sum, plural(unknown, "statement")+" not analysed")
	}
	if len(sum) == 0 {
		sum = append(sum, "nothing found")
	}
	fmt.Fprintf(w, "\n%s\n", strings.Join(sum, ", "))
	return worst
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// Measurement writes a run, statement by statement, then the summary.
func Measurement(w io.Writer, name string, run *measure.Run, verbose bool) {
	server, _, _ := strings.Cut(run.Server, " ")
	fmt.Fprintf(w, "%s on %s (PostgreSQL %s)\n", name, run.Database, server)
	var rows []string
	for _, t := range run.Tables {
		rows = append(rows, fmt.Sprintf("%s %s rows", t, commas(run.Rows[t])))
	}
	fmt.Fprintf(w, "tables: %s\n", strings.Join(rows, ", "))
	if len(run.Classes) > 0 {
		var ws []string
		for _, c := range run.Classes {
			ws = append(ws, fmt.Sprintf("%s ×%d (p99 %s)", c, run.Workers[c], Dur(run.Baseline[c].P99)))
		}
		fmt.Fprintf(w, "workload: %s\n", strings.Join(ws, ", "))
	}
	for i, st := range run.Statements {
		if st.Skipped {
			fmt.Fprintf(w, "\nline %-4d %s\n          skipped: the transaction was rolled back\n", st.Line, oneLine(st.SQL, 72))
			continue
		}
		r := run.Lint[i]
		if r.TxControl && !verbose {
			continue
		}
		fmt.Fprintf(w, "\nline %-4d %s\n", st.Line, oneLine(st.SQL, 72))
		if st.LongReader != nil {
			fmt.Fprintf(w, "          (a transaction holding ACCESS SHARE on %s for %s started 1 s before this)\n", st.LongReader.Table, Dur(st.LongReader.Hold))
		}
		var facts []string
		facts = append(facts, "took "+Dur(st.Took))
		if st.Batches > 0 {
			facts = append(facts, fmt.Sprintf("%d batches", st.Batches))
		}
		if !st.LocksExact && st.LockSamples == 0 && st.Error == "" {
			facts = append(facts, "too quick for the lock sampler")
		}
		for _, t := range sortedKeys(st.Locks) {
			if st.Locks[t] > lint.AccessShare {
				facts = append(facts, fmt.Sprintf("%s on %s", st.Locks[t], t))
			}
		}
		for _, t := range st.Rewritten {
			facts = append(facts, "rewrote "+t)
		}
		for _, t := range st.Scanned {
			if !contains(st.Rewritten, t) {
				facts = append(facts, "scanned "+t)
			}
		}
		if st.LockWait >= 50*time.Millisecond {
			facts = append(facts, "waited "+Dur(st.LockWait)+" for its lock")
		}
		fmt.Fprintf(w, "          %s\n", strings.Join(facts, " · "))
		if st.Error != "" {
			tag := "FAILED"
			if st.Expected {
				tag = "failed as expected"
			}
			fmt.Fprintf(w, "          %s: %s\n", tag, st.Error)
		}
		if sev := r.Severity(); sev >= lint.Risk {
			fmt.Fprintf(w, "          lint: %s\n", sev)
		}
		if len(run.Classes) > 0 {
			var cs []string
			for _, c := range run.Classes {
				cs = append(cs, fmt.Sprintf("%s %s", c, Dur(st.Classes[c].Max)))
			}
			fmt.Fprintf(w, "          longest query: %s\n", strings.Join(cs, ", "))
		}
		var bl []string
		for _, t := range run.Tables {
			b := st.Blocked[t]
			if b.Reads >= 20*time.Millisecond || b.Writes >= 20*time.Millisecond {
				bl = append(bl, fmt.Sprintf("%s reads %s, writes %s", t, Dur(b.Reads), Dur(b.Writes)))
			}
		}
		if len(bl) > 0 {
			fmt.Fprintf(w, "          locked out: %s\n", strings.Join(bl, "; "))
		}
	}
	Summary(w, run)
}

// Summary writes the whole-migration result.
func Summary(w io.Writer, run *measure.Run) {
	fmt.Fprintf(w, "\nmigration: %s", Dur(run.To-run.From))
	if run.Failed != "" {
		fmt.Fprintf(w, ", FAILED at %s", run.Failed)
	}
	fmt.Fprintln(w)
	if len(run.Classes) > 0 {
		var cs []string
		for _, c := range run.Classes {
			cs = append(cs, fmt.Sprintf("%s %s", c, Dur(run.Overall[c].Max)))
		}
		fmt.Fprintf(w, "longest query during it: %s\n", strings.Join(cs, ", "))
	}
	tot := run.BlockedTotal()
	var bl []string
	for _, t := range run.Tables {
		b := tot[t]
		bl = append(bl, fmt.Sprintf("%s reads %s, writes %s", t, Dur(b.Reads), Dur(b.Writes)))
	}
	fmt.Fprintf(w, "locked out (lock probes): %s\n", strings.Join(bl, "; "))
	dis, un := measure.Agree(run)
	switch {
	case len(dis) == 0 && len(un) == 0:
		fmt.Fprintln(w, "lint: agrees with every measured lock, rewrite and scan")
	case len(dis) == 0:
		fmt.Fprintf(w, "lint: agrees where it analysed; %s not analysed\n", plural(len(un), "statement"))
	default:
		fmt.Fprintf(w, "lint: DISAGREES with the measurement:\n")
		for _, d := range dis {
			fmt.Fprintf(w, "  line %d: %s\n", d.Line, d.What)
		}
	}
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func sortedKeys(m map[string]lint.Mode) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
