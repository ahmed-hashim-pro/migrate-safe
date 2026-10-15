package measure

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Workload is the application traffic replayed while the migration runs.
//
// A workload file looks like this:
//
//	-- param: id = orders.id
//	-- class: read workers=4
//	SELECT status FROM orders WHERE id = :id;
//
// Each :param is replaced, per query, by a random integer between the
// column's minimum and maximum when the run starts.
type Workload struct {
	Classes []Class
	Params  []Param
	Think   time.Duration // pause between one worker's queries
}

// Class is one kind of query, run by its own workers.
type Class struct {
	Name    string
	SQL     string
	Workers int
}

// Param is a placeholder filled from a column's range.
type Param struct {
	Name, Table, Column string
	lo, hi              int64
}

// ParseWorkload reads a workload file.
func ParseWorkload(src string) (*Workload, error) {
	w := &Workload{Think: 2 * time.Millisecond}
	var cur *Class
	var sql strings.Builder
	sc := bufio.NewScanner(strings.NewReader(src))
	ln := 0
	flush := func() error {
		if cur == nil {
			if strings.TrimSpace(sql.String()) != "" {
				return fmt.Errorf("line %d: SQL before any `-- class:` line", ln)
			}
			return nil
		}
		q := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql.String()), ";"))
		if q == "" {
			return fmt.Errorf("class %s has no SQL", cur.Name)
		}
		cur.SQL = q
		w.Classes = append(w.Classes, *cur)
		cur = nil
		sql.Reset()
		return nil
	}
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "-- param:"):
			name, ref, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "-- param:")), "=")
			t, c, ok2 := strings.Cut(strings.TrimSpace(ref), ".")
			if !ok || !ok2 {
				return nil, fmt.Errorf("line %d: want `-- param: name = table.column`", ln)
			}
			w.Params = append(w.Params, Param{Name: strings.TrimSpace(name), Table: t, Column: c})
		case strings.HasPrefix(line, "-- think:"):
			d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(line, "-- think:")))
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", ln, err)
			}
			w.Think = d
		case strings.HasPrefix(line, "-- class:"):
			if err := flush(); err != nil {
				return nil, err
			}
			f := strings.Fields(strings.TrimPrefix(line, "-- class:"))
			if len(f) == 0 {
				return nil, fmt.Errorf("line %d: class needs a name", ln)
			}
			cur = &Class{Name: f[0], Workers: 1}
			for _, kv := range f[1:] {
				k, v, _ := strings.Cut(kv, "=")
				n, err := strconv.Atoi(v)
				if k != "workers" || err != nil || n < 1 {
					return nil, fmt.Errorf("line %d: unknown class option %q", ln, kv)
				}
				cur.Workers = n
			}
		case strings.HasPrefix(line, "--") || line == "":
		default:
			sql.WriteString(sc.Text() + "\n")
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(w.Classes) == 0 {
		return nil, fmt.Errorf("workload has no `-- class:` sections")
	}
	return w, nil
}

// SetWorkers overrides worker counts, e.g. "read=8,write=2".
func (w *Workload) SetWorkers(spec string) error {
	for _, kv := range strings.Split(spec, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("bad worker count %q", kv)
		}
		found := false
		for i := range w.Classes {
			if w.Classes[i].Name == k {
				w.Classes[i].Workers, found = n, true
			}
		}
		if !found {
			return fmt.Errorf("no workload class %q", k)
		}
	}
	return nil
}

// Query is one finished workload query.
type Query struct {
	Class      int
	Start, End time.Duration // since the run's origin
	Err        bool
	Cancelled  bool // still running when the run ended
}

type recorder struct {
	mu      sync.Mutex
	queries []Query
}

func (r *recorder) add(q Query) {
	r.mu.Lock()
	r.queries = append(r.queries, q)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []Query {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Query(nil), r.queries...)
}

func (w *Workload) resolve(ctx context.Context, conn *pgx.Conn) error {
	for i := range w.Params {
		p := &w.Params[i]
		q := fmt.Sprintf("SELECT coalesce(min(%s),0)::bigint, coalesce(max(%s),0)::bigint FROM %s",
			pgx.Identifier{p.Column}.Sanitize(), pgx.Identifier{p.Column}.Sanitize(), pgx.Identifier{p.Table}.Sanitize())
		if err := conn.QueryRow(ctx, q).Scan(&p.lo, &p.hi); err != nil {
			return fmt.Errorf("workload param %s: %w", p.Name, err)
		}
	}
	return nil
}

var paramRe = regexp.MustCompile(`(^|[^:]):([a-zA-Z_][a-zA-Z0-9_]*)`)

func (w *Workload) render(sql string, rng *rand.Rand) string {
	return paramRe.ReplaceAllStringFunc(sql, func(m string) string {
		lead := ""
		if m[0] != ':' {
			lead, m = m[:1], m[1:]
		}
		for _, p := range w.Params {
			if p.Name == m[1:] {
				v := p.lo
				if p.hi > p.lo {
					v += rng.Int64N(p.hi - p.lo + 1)
				}
				return lead + strconv.FormatInt(v, 10)
			}
		}
		return lead + m
	})
}

// run starts every worker and returns when ctx is cancelled and all
// in-flight queries have returned.
func (w *Workload) run(ctx context.Context, pool *pgxpool.Pool, origin time.Time, rec *recorder) {
	var wg sync.WaitGroup
	seed := uint64(1)
	for ci, c := range w.Classes {
		for k := 0; k < c.Workers; k++ {
			seed++
			wg.Add(1)
			go func(ci int, sql string, seed uint64) {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(seed, 42))
				for ctx.Err() == nil {
					q := w.render(sql, rng)
					s := time.Since(origin)
					_, err := pool.Exec(ctx, q)
					if ctx.Err() != nil {
						// Still running when the run ended: keep it, since a query
						// stuck behind a lock is exactly what is being measured.
						rec.add(Query{Class: ci, Start: s, End: time.Since(origin), Cancelled: true})
						return
					}
					rec.add(Query{Class: ci, Start: s, End: time.Since(origin), Err: err != nil})
					if w.Think > 0 {
						select {
						case <-ctx.Done():
						case <-time.After(w.Think):
						}
					}
				}
			}(ci, c.SQL, seed)
		}
	}
	wg.Wait()
}

// Workers is the total number of workers.
func (w *Workload) Workers() int {
	n := 0
	for _, c := range w.Classes {
		n += c.Workers
	}
	return n
}
