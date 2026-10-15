# migrate-safe

[![CI](https://github.com/ahmed-hashim-pro/migrate-safe/actions/workflows/ci.yml/badge.svg)](https://github.com/ahmed-hashim-pro/migrate-safe/actions/workflows/ci.yml) [![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Find out what a PostgreSQL migration will do to a live database before it
ships. `migrate-safe measure` copies the database, replays application
traffic against the copy, runs the migration on it, and records what actually
happened: the lock each statement held on each table, whether it rewrote or
scanned the table while holding it, how long it waited in the lock queue, and
how long reads and writes were locked out. `migrate-safe lint` predicts the
same facts in milliseconds without a database, and its predictions are
checked against the measurements on every CI run.

Most outages caused by migrations come from statements that look harmless.
`CREATE INDEX` is one line. `ADD COLUMN ... DEFAULT gen_random_uuid()` looks
like `ADD COLUMN ... DEFAULT 0`, but one rewrites the table under ACCESS
EXCLUSIVE and the other changes only the catalog. `SET NOT NULL` reads every
row while nothing else can read the table. Even a statement that finishes in a
millisecond can stall all traffic if it has to wait behind a long-running
report. What a statement locks, and whether it does work proportional to the
table while holding that lock, depends on the PostgreSQL version and on the
current schema. Linters guess from rules. This tool runs the migration on a
copy of the data and checks.

```
$ migrate-safe lint examples/migrations/03-set-not-null/unsafe.sql
examples/migrations/03-set-not-null/unsafe.sql (PostgreSQL 17)

line 2    ALTER TABLE orders ALTER COLUMN status SET NOT NULL
          ACCESS EXCLUSIVE on orders · reads every row
  DANGER  reads the whole table while holding ACCESS EXCLUSIVE on orders: reads and writes on orders wait until it finishes
          fix: ADD CONSTRAINT ... CHECK (status IS NOT NULL) NOT VALID, then VALIDATE CONSTRAINT, then SET NOT NULL (no scan), then drop the check
  RISK    takes ACCESS EXCLUSIVE on orders with no lock_timeout: if any transaction is using orders, this waits, and reads and writes on orders queue behind it
          fix: SET lock_timeout = '2s' before it, and retry the migration if it times out

1 danger, 1 risk
```

## Six migrations, measured

Each example in [`examples/migrations`](examples/migrations) is a change as
it is usually written (`unsafe.sql`) and the same change rewritten so it
doesn't block traffic (`safe.sql`). `migrate-safe examples` runs all twelve on
copies of a database with 5 million orders while ten workers read, update
and insert orders the whole time:

![How long each migration locked out reads or writes on a table, on 5 million rows. As usually written: adding a column with a volatile default locked out reads and writes for 50 seconds and changing a column type for 34 seconds; waiting behind an open report, a metadata-only ADD COLUMN locked out reads and writes for 7 seconds; CREATE INDEX and ADD FOREIGN KEY locked out writes for 1.7 and 1.0 seconds; SET NOT NULL locked out reads and writes for 0.4 seconds. Rewritten, each locked out traffic for at most 0.2 seconds, except the column-type change, which waited 1 second behind autovacuum.](docs/examples.svg)

| Example | Lint, as written | Locked out, as written | Locked out, rewritten | The rewrite |
| --- | --- | --- | --- | --- |
| [create-index](examples/migrations/01-create-index) | DANGER | writes **1.73 s** | none | `CONCURRENTLY` (2.65 s in all) |
| [add-foreign-key](examples/migrations/02-add-foreign-key) | DANGER | writes **1.02 s** | writes 38 ms | `NOT VALID`, then `VALIDATE` |
| [set-not-null](examples/migrations/03-set-not-null) | DANGER | reads and writes **376 ms** | 57 ms | `CHECK ... NOT VALID`, `VALIDATE`, `SET NOT NULL` |
| [add-column-volatile-default](examples/migrations/04-add-column-volatile-default) | DANGER | reads and writes **50.4 s** | 19 ms | add empty, `SET DEFAULT`, backfill in 250 batches (182 s in all) |
| [change-column-type](examples/migrations/05-change-column-type) | DANGER | reads and writes **33.8 s** | 1.06 s (autovacuum, below) | new column, trigger, backfill, swap (200 s in all) |
| [lock-queue](examples/migrations/06-lock-queue) | RISK | reads and writes **6.99 s** | 181 ms | `lock_timeout = '200ms'`; it fails and is retried later |

The linter's verdict for every rewritten version is "nothing found". The
full table, including the slowest query of each run, is in
[`docs/examples.txt`](docs/examples.txt).

*Measured on PostgreSQL 17.11 in Docker, on an i5-12400F shared with
other work. "Locked out" comes from lock probes: every 20 ms a separate
session tries to take each table's read lock (ACCESS SHARE) and write lock
(ROW EXCLUSIVE) with NOWAIT, so it counts only time lost to table locks.
The slowest workload query also counts disk and CPU contention: during the
batched backfills, queries occasionally took 4 to 10 s while the probes found
no lock in the way. A first run of the same examples while the machine was
heavily loaded by other work gave rewrites of about 62 s and stalls of up to
38 s during the backfills, again with no lock behind them. The full
measurements are in [`docs/examples.json`](docs/examples.json).*

What the numbers show:

- **A rewrite holds ACCESS EXCLUSIVE for as long as it takes to copy the
  table.** On 5 million rows that was 34 s for the type change and 50 s for the
  volatile default, with every query on `orders` stuck for all of it. The rewritten versions add the column empty,
  fill it in batches of 20,000 rows that each commit on their own, and swap
  columns in one short transaction.
- **`CREATE INDEX` and `ADD FOREIGN KEY` block writes, not reads.** That is
  easy to miss in a test environment where nothing is writing. `CONCURRENTLY`
  and `NOT VALID` + `VALIDATE CONSTRAINT` take locks that don't conflict with
  writes, and took longer overall.
- **A metadata-only change can still stall everything.** Adding a column with
  a constant default doesn't touch the rows on PostgreSQL 11+. But it needs
  ACCESS EXCLUSIVE, and while it waits for a report that has `orders` open,
  every query that arrives after it queues behind it. With
  `lock_timeout = '200ms'` the ALTER gives up instead, and the pile-up lasts
  200 ms. The deploy has to retry it later.
- **Autovacuum holds locks too.** After the column-type rewrite backfilled
  5 million rows, autovacuum started on `orders`. The next statement, a
  metadata-only `ADD CONSTRAINT ... NOT VALID`, waited exactly 1.00 s for its
  lock: `deadlock_timeout`, after which PostgreSQL cancelled the autovacuum
  (the server log says `canceling autovacuum task`). Traffic queued behind the
  ALTER for that second. `lock_timeout = '2s'` allowed it; a shorter timeout
  would have failed the statement and needed a retry. The same 1.0 s wait
  appeared at 1 million rows.
- **The linter agreed with every measurement**: for every statement, the
  strongest lock on each table, whether each table was rewritten, whether it
  was scanned, and, in the lock-queue example, whether the statement waited
  and stalled traffic.

![Terminal recording: migrate-safe lint flags CREATE INDEX as DANGER and RISK; migrate-safe measure runs it on a copy of a 2-million-row database under live traffic and shows writes locked out for the whole build; the CONCURRENTLY version shows no lock-out.](docs/demo.gif)

## Quickstart

You need Docker. Go 1.24+ is used if installed; otherwise the Makefile builds
with the `golang:1.25` image.

```bash
git clone https://github.com/ahmed-hashim-pro/migrate-safe.git
cd migrate-safe
make build

# Static analysis, no database needed:
./bin/migrate-safe lint examples/migrations/*/unsafe.sql

# A throwaway PostgreSQL 17 on port 55432:
make pg

# Measure all twelve examples on 1,000,000 orders (about 3 minutes):
./bin/migrate-safe examples --dsn postgres://postgres:pg@localhost:55432/postgres --rows 1000000

# Or one migration against the demo database, with full detail:
./bin/migrate-safe measure --dsn postgres://postgres:pg@localhost:55432/migrate_safe_demo \
    examples/migrations/02-add-foreign-key/unsafe.sql

make pg-stop
```

On your own database, point `--dsn` at a restored copy of production (or a
staging database with production-sized tables) and give it a workload file
that looks like your traffic:

```bash
migrate-safe lint --schema schema.sql migrations/0042_add_index.sql
migrate-safe measure --dsn postgres://.../staging --workload traffic.sql migrations/0042_add_index.sql
```

## What `measure` records

`measure` copies the database with `CREATE DATABASE ... TEMPLATE` (which needs
no other sessions on the source; use `--in-place` on a database you can throw
away), starts the workload, waits two seconds for a baseline, then runs the
migration one statement at a time:

| Fact | How it is measured |
| --- | --- |
| Locks held, per table | Each statement runs inside a transaction, and `pg_locks` is read for the migration's backend before COMMIT, so every lock it held is still there. Wrapping a single statement doesn't change its locking: a lone statement holds its locks until it commits either way. Statements that refuse to run in a transaction (`CONCURRENTLY`, `VACUUM`) are sampled from another session every 5 ms; one that finishes between two samples is reported as too quick to observe, and its locks are not compared. |
| Rewrite | `pg_class.relfilenode` before and after. A rewrite writes the table out to a new file. |
| Full scan | `seq_scan` in `pg_stat_xact_user_tables` (this transaction's own counters) before and after. For statements outside a transaction, the shared counters after `pg_stat_force_next_flush()` (PostgreSQL 15+). |
| Rows touched | The command tag. |
| Lock-queue wait | `pg_locks.granted = false` for the migration's backend, sampled every 5 ms. |
| Reads and writes locked out | Every 20 ms another session runs `LOCK TABLE t IN ACCESS SHARE MODE NOWAIT` and `... ROW EXCLUSIVE MODE NOWAIT` and rolls back. NOWAIT fails when a conflicting lock is held, and also when one is *queued* ahead of it, so this sees the pile-up behind a waiting ACCESS EXCLUSIVE, not just the lock itself. It sees table locks only. |
| Query latency | Every workload query's start and end, summarised per statement and class. |

`--json` prints all of it.

## What `lint` predicts

The linter reads a migration, tracks the schema and the session's
`lock_timeout` as the migration changes them, and for every statement predicts
the same facts `measure` records. It is not a SQL parser, and it says so: a
statement it doesn't understand is reported as *not analysed*, never as safe.

| Statement | Lock | Scans or rewrites | Safe rewrite |
| --- | --- | --- | --- |
| `CREATE INDEX` | SHARE: blocks writes | scans | `CREATE INDEX CONCURRENTLY` (SHARE UPDATE EXCLUSIVE) |
| `ADD COLUMN ... DEFAULT <volatile>` (`gen_random_uuid()`, `clock_timestamp()`, serial, identity, stored generated) | ACCESS EXCLUSIVE | rewrites | add empty, `SET DEFAULT`, batch backfill |
| `ADD COLUMN ... DEFAULT <constant or stable>` | ACCESS EXCLUSIVE | nothing on PG 11+; rewrites on PG ≤ 10 | `lock_timeout` |
| `ALTER COLUMN ... TYPE` | ACCESS EXCLUSIVE | rewrites, except binary-compatible changes: varchar(n) to a larger n, to unbounded varchar or text, numeric precision up at the same scale | new column, trigger, batch backfill, swap |
| `ALTER COLUMN ... SET NOT NULL` | ACCESS EXCLUSIVE | scans, unless a validated `CHECK (col IS NOT NULL)` exists (PG 12+) | `CHECK ... NOT VALID`, `VALIDATE`, `SET NOT NULL`, drop the check |
| `ADD CONSTRAINT ... CHECK` | ACCESS EXCLUSIVE | scans, unless `NOT VALID` | `NOT VALID`, then `VALIDATE CONSTRAINT` (SHARE UPDATE EXCLUSIVE) |
| `ADD CONSTRAINT ... FOREIGN KEY` | SHARE ROW EXCLUSIVE on **both** tables | scans both, unless `NOT VALID` | `NOT VALID`, then `VALIDATE` (ROW SHARE on the referenced table) |
| `ADD CONSTRAINT ... UNIQUE / PRIMARY KEY` | ACCESS EXCLUSIVE | builds an index | `CREATE UNIQUE INDEX CONCURRENTLY`, then `... USING INDEX` |
| `UPDATE` / `DELETE` with no `WHERE` | ROW EXCLUSIVE | locks every row until commit | `-- migrate-safe: batch` |
| `CREATE TABLE ... REFERENCES t`, `DROP TABLE` with foreign keys, `DROP CONSTRAINT` (FK) | also lock the *other* table | — | `lock_timeout` |

It also knows `DROP INDEX [CONCURRENTLY]`, `REINDEX`, `RENAME`, `DROP COLUMN`,
`SET/DROP DEFAULT`, `SET STATISTICS`, storage parameters, triggers,
`TRUNCATE`, `VACUUM [FULL]`, `CLUSTER`, `LOCK TABLE`, and `SET LOCAL`. Inside
`BEGIN ... COMMIT` it accumulates locks: an ACCESS EXCLUSIVE taken by a cheap
statement is still held by the slow one after it.

Every statement then gets a verdict:

- **DANGER**: it holds a lock that blocks reads or writes while doing work
  proportional to the table (a rewrite, a scan, or row locks on every row).
- **RISK**: it asks for a lock that blocks reads or writes with no
  `lock_timeout` set, so a long transaction can make it wait while traffic
  queues behind it.

`lint` exits 1 on either (`--fail-on danger` to allow RISK). `--pg N` sets the
version, and `--schema file.sql` gives it column types and constraints;
without them it assumes the worst and says so.

### Checked against PostgreSQL

The linter's table is not from the documentation alone. CI runs, on
PostgreSQL 16 and 17:

- [`internal/measure/testdata/corpus.sql`](internal/measure/testdata/corpus.sql):
  67 statements of every kind above, run one after another on a real database. Each
  prediction (lock per table, rewrite, scan, rows) must match `pg_locks`,
  `relfilenode` and `seq_scan` exactly.
- every example, as written and rewritten, under traffic, including the
  lock-queue wait and its pile-up.
- a check of the measurement itself: a transaction that holds ACCESS
  EXCLUSIVE for one second must show reads and writes locked out for about
  one second, in both the probes and the workload.

The corpus caught two of the linter's mistakes while it was being written:
`DROP TABLE` on a table with a foreign key also takes ACCESS EXCLUSIVE on the
table it references (it removes that table's FK triggers), and it was
comparing a table created by the same statement.

## Directives

Comments of the form `-- migrate-safe: <key> <value>` just before a statement:

| Directive | Effect |
| --- | --- |
| `batch orders.id 20000` | Run the statement once per range of `orders.id`, replacing `:lo` and `:hi`, each in its own transaction. The linter then treats it as bounded. |
| `long-reader orders 8s` | Before the statement, open a transaction that holds ACCESS SHARE on `orders` for 8 s, starting 1 s early. Simulates a report or a long-running query. |
| `expect-error 55P03` | The statement is expected to fail with this SQLSTATE (55P03 is `lock_not_available`, what `lock_timeout` raises); the run continues. |

## Workload files

```sql
-- param: id = orders.id          -- :id is a random value between min(id) and max(id)
-- think: 2ms                     -- pause between one worker's queries

-- class: read workers=4
SELECT id, status, amount_cents FROM orders WHERE id = :id;

-- class: write workers=4
UPDATE orders SET status = 'shipped' WHERE id = :id;
```

`--workers read=8,write=2` overrides the counts. `--workload none` runs the
migration with no traffic.

## What this does not do

- **It doesn't make the copy behave like production.** The copy has
  production's data if you restore it, but not its cache, hardware, or
  traffic. Durations scale with your hardware; the locks, rewrites and scans
  don't. Treat the times as orders of magnitude.
- **It replays the traffic you describe.** The workload is a file of query
  shapes with random keys, not a capture of production traffic.
- **The probes see table locks only.** A row lock held by a batch shows up
  in workload latency, not in "locked out".
- **It doesn't retry.** With `lock_timeout` the right response to 55P03 is
  to retry later; that is the deploy tool's job.
- **The linter covers the statements listed above.** Partitions (`ATTACH`,
  `DETACH`), `ALTER TYPE`, materialized views, `GRANT`, and `DO` blocks are
  reported as not analysed. Volatility is judged from a list of known
  functions; an unknown function in a default is assumed volatile.
- **It doesn't measure WAL volume, replication lag, or disk space**, all of
  which a rewrite also costs.
- **Tested on PostgreSQL 16 and 17.** The version rules for 10, 11 and 12
  are in the linter and its unit tests, but those servers were not run.
  Scan detection for statements outside a transaction needs 15+.

## Layout

```
cmd/migrate-safe/      the CLI: lint, measure, seed, examples
internal/sqlparse/     statement splitting (quotes, dollar quoting, comments, directives) and tokens
internal/lint/         lock modes and conflicts, schema tracking, the rules
internal/measure/      shadow copy, workload, lock probes, pg_locks sampler, agreement check
internal/report/       text output and the SVG chart
examples/              schema.sql, workload.sql, migrations/*/{unsafe,safe}.sql
```

## License

MIT
