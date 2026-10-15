package measure

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
)

// ProbeSample is one lock probe: could a reader (ACCESS SHARE) or a writer
// (ROW EXCLUSIVE) have taken its lock on Table at time At?
type ProbeSample struct {
	At      time.Duration
	Table   string
	Writes  bool // false: the read probe
	Blocked bool
}

// prober tries, every tick, to take each table's read and write lock with
// NOWAIT inside a transaction it immediately rolls back. A NOWAIT request
// fails both when a conflicting lock is held and when one is queued ahead of
// it, so the probe also sees the lock-queue pile-up behind a waiting
// ACCESS EXCLUSIVE.
type prober struct {
	conn    *pgx.Conn
	tables  []string
	tick    time.Duration
	origin  time.Time
	mu      sync.Mutex
	samples []ProbeSample
}

func (p *prober) run(ctx context.Context) error {
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		for _, tbl := range p.tables {
			for _, writes := range []bool{false, true} {
				mode := "ACCESS SHARE"
				if writes {
					mode = "ROW EXCLUSIVE"
				}
				at := time.Since(p.origin)
				_, err := p.conn.Exec(ctx, fmt.Sprintf("BEGIN; LOCK TABLE %s IN %s MODE NOWAIT; ROLLBACK", pgx.Identifier{tbl}.Sanitize(), mode))
				if ctx.Err() != nil {
					return nil
				}
				blocked := false
				if err != nil {
					var pe *pgconn.PgError
					if !errors.As(err, &pe) || pe.Code != "55P03" {
						if errors.As(err, &pe) && pe.Code == "42P01" {
							continue // the table was dropped or renamed by the migration
						}
						return fmt.Errorf("lock probe on %s: %w", tbl, err)
					}
					blocked = true
					if _, err := p.conn.Exec(ctx, "ROLLBACK"); err != nil && ctx.Err() == nil {
						return err
					}
				}
				p.mu.Lock()
				p.samples = append(p.samples, ProbeSample{At: at, Table: tbl, Writes: writes, Blocked: blocked})
				p.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// LockSample is the migration backend's table locks at one moment.
type LockSample struct {
	At      time.Duration
	Stmt    int                  // index of the statement running
	Granted map[string]lint.Mode // strongest granted lock per table
	Waiting bool                 // waiting for a lock
}

// sampler polls pg_locks and pg_stat_activity for the migration's backend.
// It is the only source of locks for statements that cannot run inside a
// transaction, and the source of lock-wait time for every statement.
type sampler struct {
	conn    *pgx.Conn
	pid     uint32
	tick    time.Duration
	origin  time.Time
	current func() int
	mu      sync.Mutex
	samples []LockSample
}

const lockQuery = `
SELECT c.relname, l.mode, l.granted
FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
WHERE l.pid = $1 AND l.locktype = 'relation' AND c.relkind IN ('r', 'p')
  AND c.relnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)`

func (s *sampler) run(ctx context.Context) error {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		stmt := s.current()
		at := time.Since(s.origin)
		rows, err := s.conn.Query(ctx, lockQuery, s.pid)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock sampler: %w", err)
		}
		ls := LockSample{At: at, Stmt: stmt, Granted: map[string]lint.Mode{}}
		for rows.Next() {
			var rel, mode string
			var granted bool
			if err := rows.Scan(&rel, &mode, &granted); err != nil {
				return err
			}
			m, _ := lint.ParsePGLock(mode)
			if !granted {
				ls.Waiting = true
				continue
			}
			if m > ls.Granted[rel] {
				ls.Granted[rel] = m
			}
		}
		if err := rows.Err(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// A sample is only kept if the same statement was running when the
		// query was sent and when its answer came back; otherwise a lock
		// taken by the next statement could be credited to this one.
		if s.current() == stmt {
			s.mu.Lock()
			s.samples = append(s.samples, ls)
			s.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// blockedIn sums the time within [from, to) for which a probe of the given
// kind on table was failing. A failing sample counts until the next sample of
// the same kind.
func blockedIn(samples []ProbeSample, table string, writes bool, from, to time.Duration) time.Duration {
	var total time.Duration
	var prev *ProbeSample
	for i := range samples {
		s := &samples[i]
		if s.Table != table || s.Writes != writes {
			continue
		}
		if prev != nil && prev.Blocked {
			a, b := max(prev.At, from), min(s.At, to)
			if b > a {
				total += b - a
			}
		}
		prev = s
	}
	if prev != nil && prev.Blocked && to > max(prev.At, from) {
		total += to - max(prev.At, from)
	}
	return total
}
