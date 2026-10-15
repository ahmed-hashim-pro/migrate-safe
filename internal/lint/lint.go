// Package lint predicts what each statement of a migration does to a live
// PostgreSQL database: which lock it takes on which table, whether it
// rewrites or scans the whole table while holding that lock, and whether it
// can wait in the lock queue with nothing to stop it.
//
// The predictions are written so they can be checked: the measure package
// observes the same four facts on a real server, and `migrate-safe examples
// --check` fails if any prediction disagrees with what was observed.
package lint

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/sqlparse"
)

// Effect is what one statement does to one table.
type Effect struct {
	Table   string
	Lock    Mode
	Rewrite bool // the table (and its indexes) are written out again
	Scan    bool // every row is read while the lock is held
	// ScanKnown is false for DML, where whether a scan happens depends on
	// the plan; scans are then neither claimed nor checked.
	ScanKnown bool
	AllRows   bool // DML that row-locks every row until commit
	// AllRowsKnown is false for DML with a WHERE clause the linter can't judge.
	AllRowsKnown bool
	Why          []string
}

// Severity of a finding.
type Severity int

const (
	OK Severity = iota
	Info
	Risk   // can queue behind other transactions and stall traffic while it waits
	Danger // blocks reads or writes for as long as it takes to process the whole table
)

func (s Severity) String() string {
	return [...]string{"ok", "info", "RISK", "DANGER"}[s]
}

// Finding is one thing worth telling the author.
type Finding struct {
	Severity Severity
	Table    string
	Message  string
	Fix      string
}

// Result is the linter's prediction for one statement.
type Result struct {
	Stmt    sqlparse.Statement
	Kind    string
	Effects []Effect
	// Held is the strongest lock held on each table once this statement has
	// run: inside an explicit transaction, locks pile up until COMMIT.
	Held        map[string]Mode
	NoTx        bool // cannot run inside a transaction block (CONCURRENTLY, VACUUM)
	InTx        bool // runs inside an explicit BEGIN ... COMMIT
	TxControl   bool // BEGIN, COMMIT, ROLLBACK
	LockTimeout time.Duration
	Batch       *Batch
	Findings    []Finding
	Unknown     string // why the statement was not understood, if it wasn't
	Notes       []string
	NewTables   map[string]bool // tables created earlier in this migration
}

// Severity is the worst finding's severity.
func (r Result) Severity() Severity {
	s := OK
	for _, f := range r.Findings {
		s = max(s, f.Severity)
	}
	return s
}

// Batch is a `-- migrate-safe: batch table.column size` directive: the
// statement runs once per range [:lo, :hi) of the column, each in its own
// transaction.
type Batch struct {
	Table, Column string
	Size          int64
}

// ParseBatch parses the directive value "orders.id 10000".
func ParseBatch(v string) (*Batch, error) {
	f := strings.Fields(v)
	if len(f) != 2 {
		return nil, fmt.Errorf("batch directive wants `table.column size`, got %q", v)
	}
	t, c, ok := strings.Cut(f[0], ".")
	size, err := strconv.ParseInt(f[1], 10, 64)
	if !ok || err != nil || size <= 0 {
		return nil, fmt.Errorf("batch directive wants `table.column size`, got %q", v)
	}
	return &Batch{Table: strings.ToLower(t), Column: strings.ToLower(c), Size: size}, nil
}

// Options control the linter.
type Options struct {
	PG     int     // server major version, e.g. 17
	Schema *Schema // what the database looks like before the migration; may be empty
}

// DefaultPG is assumed when no version is given.
const DefaultPG = 17

// File lints every statement of a migration in order. The schema is not
// modified; the linter works on a copy it updates as the migration runs.
func File(stmts []sqlparse.Statement, opt Options) []Result {
	if opt.PG == 0 {
		opt.PG = DefaultPG
	}
	if opt.Schema == nil {
		opt.Schema = NewSchema()
	}
	l := &linter{pg: opt.PG, schema: opt.Schema.clone(), created: map[string]bool{}, held: map[string]Mode{}}
	var out []Result
	for _, s := range stmts {
		out = append(out, l.statement(s))
	}
	return out
}

// ApplySchema runs a schema file (CREATE TABLE, CREATE INDEX, ALTER TABLE
// ... ADD CONSTRAINT) through the linter to learn the starting schema.
func ApplySchema(stmts []sqlparse.Statement, pg int) *Schema {
	if pg == 0 {
		pg = DefaultPG
	}
	l := &linter{pg: pg, schema: NewSchema(), created: map[string]bool{}, held: map[string]Mode{}}
	for _, s := range stmts {
		l.statement(s)
	}
	return l.schema
}

type linter struct {
	pg       int
	schema   *Schema
	created  map[string]bool // tables created by this migration: nobody else is using them yet
	inTx     bool
	held     map[string]Mode
	session  time.Duration // SET lock_timeout
	local    time.Duration // SET LOCAL lock_timeout
	hasLocal bool
	before   map[string]Mode // locks held in the transaction before the current statement
}

func (l *linter) heldBefore(t string) Mode { return l.before[t] }

func (l *linter) lockTimeout() time.Duration {
	if l.inTx && l.hasLocal {
		return l.local
	}
	return l.session
}

// stmt collects one statement's analysis.
type stmt struct {
	l       *linter
	effects map[string]*Effect
	order   []string
	fixes   []fix
	notes   []string
	unknown string
	notx    bool
	kind    string
}

type fix struct{ table, text string }

func (s *stmt) effect(table string) *Effect {
	if e, ok := s.effects[table]; ok {
		return e
	}
	e := &Effect{Table: table, ScanKnown: true, AllRowsKnown: true}
	s.effects[table] = e
	s.order = append(s.order, table)
	return e
}

// lock records that the statement takes mode on table, keeping the strongest.
func (s *stmt) lock(table string, m Mode, why string) *Effect {
	e := s.effect(table)
	if m > e.Lock {
		e.Lock = m
	}
	if why != "" {
		e.Why = append(e.Why, why)
	}
	return e
}

func (s *stmt) fix(table, text string) { s.fixes = append(s.fixes, fix{table, text}) }

func (l *linter) statement(st sqlparse.Statement) Result {
	s := &stmt{l: l, effects: map[string]*Effect{}}
	r := Result{Stmt: st, Held: map[string]Mode{}, NewTables: map[string]bool{}}
	for t := range l.created {
		r.NewTables[t] = true
	}
	if v, ok := st.Directive("batch"); ok {
		b, err := ParseBatch(v)
		if err != nil {
			r.Unknown = err.Error()
			return r
		}
		r.Batch = b
	}
	c := sqlparse.NewCursor(st.SQL)
	switch {
	case c.Accept("begin"), c.Accept("start", "transaction"):
		r.Kind, r.TxControl = "BEGIN", true
		l.inTx, l.held, l.hasLocal = true, map[string]Mode{}, false
		r.InTx = true
		return r
	case c.Accept("commit"), c.Accept("end"), c.Accept("rollback"), c.Accept("abort"):
		r.Kind, r.TxControl = "COMMIT", true
		r.InTx = l.inTx
		for t, m := range l.held {
			r.Held[t] = m
		}
		l.inTx, l.held, l.hasLocal = false, map[string]Mode{}, false
		return r
	case c.Accept("set"), c.Accept("reset"):
		c.I = 0
		s.kind = "SET"
		l.set(c, s)
	case c.Accept("create"):
		l.create(c, s)
	case c.Accept("alter", "table"):
		s.kind = "ALTER TABLE"
		l.alterTable(c, s)
	case c.Accept("alter", "index"):
		s.kind = "ALTER INDEX"
		s.unknown = "ALTER INDEX is not analysed"
	case c.Accept("drop"):
		l.drop(c, s)
	case c.Accept("update"):
		s.kind = "UPDATE"
		l.dml(c, s, r.Batch)
	case c.Accept("delete", "from"):
		s.kind = "DELETE"
		l.dml(c, s, r.Batch)
	case c.Accept("insert", "into"):
		s.kind = "INSERT"
		if t, ok := c.Name(); ok {
			s.lock(t, RowExclusive, "INSERT").ScanKnown = false
		}
	case c.Accept("lock"):
		s.kind = "LOCK"
		l.lockTable(c, s)
	case c.Accept("truncate"):
		s.kind = "TRUNCATE"
		c.Accept("table")
		c.Accept("only")
		for {
			t, ok := c.Name()
			if !ok {
				break
			}
			s.lock(t, AccessExclusive, "TRUNCATE")
			if !c.AcceptPunct(",") {
				break
			}
		}
	case c.Accept("vacuum"):
		s.kind = "VACUUM"
		s.notx = true
		full := false
		if g, ok := c.Group(); ok {
			for _, t := range g {
				full = full || t.Is("full")
			}
		}
		for c.Peek(0).Kind == sqlparse.Word && (c.Peek(0).Is("full") || c.Peek(0).Is("verbose") || c.Peek(0).Is("analyze") || c.Peek(0).Is("freeze")) {
			full = full || c.Next().Is("full")
		}
		if t, ok := c.Name(); ok {
			if full {
				e := s.lock(t, AccessExclusive, "VACUUM FULL rewrites the table")
				e.Rewrite, e.Scan = true, true
			} else {
				s.lock(t, ShareUpdateExclusive, "VACUUM").ScanKnown = false
			}
		}
	case c.Accept("analyze"), c.Accept("analyse"):
		s.kind = "ANALYZE"
		c.Group()
		c.Accept("verbose")
		if t, ok := c.Name(); ok {
			s.lock(t, ShareUpdateExclusive, "ANALYZE").ScanKnown = false
		}
	case c.Accept("cluster"):
		s.kind = "CLUSTER"
		c.Accept("verbose")
		if t, ok := c.Name(); ok {
			e := s.lock(t, AccessExclusive, "CLUSTER rewrites the table in index order")
			e.Rewrite, e.Scan = true, true
			s.fix(t, "pg_repack can reorder a table without holding ACCESS EXCLUSIVE for the whole rewrite")
		}
	case c.Accept("reindex"):
		s.kind = "REINDEX"
		l.reindex(c, s)
	case c.Peek(0).Is("select"), c.Peek(0).Is("with"), c.Peek(0).Is("values"):
		s.kind = "SELECT"
	case c.Accept("comment", "on"):
		s.kind = "COMMENT"
		s.unknown = "COMMENT ON is not analysed"
	case c.Accept("do"):
		s.kind = "DO"
		s.unknown = "procedural code is not analysed: write its statements out, or measure it"
	default:
		s.kind = strings.ToUpper(c.Peek(0).Text)
		s.unknown = "statement type not analysed"
	}
	return l.finish(st, s, r)
}

func (l *linter) finish(st sqlparse.Statement, s *stmt, r Result) Result {
	r.Kind, r.NoTx, r.Unknown, r.Notes = s.kind, s.notx, s.unknown, s.notes
	r.InTx = l.inTx
	r.LockTimeout = l.lockTimeout()
	if s.notx && l.inTx {
		r.Findings = append(r.Findings, Finding{Severity: Info, Message: s.kind + " cannot run inside a transaction block; PostgreSQL will reject it here"})
	}
	for _, t := range s.order {
		e := *s.effects[t]
		r.Effects = append(r.Effects, e)
	}
	l.before = map[string]Mode{}
	for t, m := range l.held {
		l.before[t] = m
	}
	// Locks taken inside an explicit transaction are held until COMMIT.
	if l.inTx {
		for _, e := range r.Effects {
			if e.Lock > l.held[e.Table] {
				l.held[e.Table] = e.Lock
			}
		}
		for t, m := range l.held {
			r.Held[t] = m
		}
	} else {
		for _, e := range r.Effects {
			r.Held[e.Table] = e.Lock
		}
	}
	if r.Batch != nil {
		r.Notes = append(r.Notes, fmt.Sprintf("runs in batches of %d over %s.%s, each committed on its own", r.Batch.Size, r.Batch.Table, r.Batch.Column))
	}
	for _, e := range r.Effects {
		if l.created[e.Table] {
			continue // a table this migration created has no other users yet
		}
		held := r.Held[e.Table]
		var blocks []string
		if held.BlocksReads() {
			blocks = append(blocks, "reads")
		}
		if held.BlocksWrites() {
			blocks = append(blocks, "writes")
		}
		slow := ""
		switch {
		case e.Rewrite:
			slow = "rewrites the whole table"
		case e.Scan:
			slow = "reads the whole table"
		}
		fixText := ""
		for _, f := range s.fixes {
			if f.table == e.Table || f.table == "" {
				fixText = f.text
				break
			}
		}
		if e.AllRows && e.AllRowsKnown && len(blocks) > 0 {
			slow = "touches every row"
		}
		switch {
		case e.AllRows && e.AllRowsKnown && len(blocks) == 0:
			r.Findings = append(r.Findings, Finding{Severity: Danger, Table: e.Table,
				Message: fmt.Sprintf("locks every row of %s until it commits: any write to %s waits for the whole statement", e.Table, e.Table),
				Fix:     firstNonEmpty(fixText, "update in batches by primary key, each batch in its own transaction (`-- migrate-safe: batch "+e.Table+".id 10000`)")})
		case slow != "" && len(blocks) > 0:
			msg := fmt.Sprintf("%s while holding %s on %s: %s on %s wait until it finishes", slow, held, e.Table, strings.Join(blocks, " and "), e.Table)
			if held != e.Lock {
				msg += fmt.Sprintf(" (the %s was taken earlier in this transaction)", held)
			}
			r.Findings = append(r.Findings, Finding{Severity: Danger, Table: e.Table, Message: msg, Fix: fixText})
		}
		// The queue risk is about the lock this statement asks for: one
		// already held earlier in the transaction is not waited for again.
		var queued []string
		if e.Lock.BlocksReads() {
			queued = append(queued, "reads")
		}
		if e.Lock.BlocksWrites() {
			queued = append(queued, "writes")
		}
		if len(queued) > 0 && e.Lock > l.heldBefore(e.Table) && r.LockTimeout == 0 && !r.TxControl {
			r.Findings = append(r.Findings, Finding{Severity: Risk, Table: e.Table,
				Message: fmt.Sprintf("takes %s on %s with no lock_timeout: if any transaction is using %s, this waits, and %s on %s queue behind it", e.Lock, e.Table, e.Table, strings.Join(queued, " and "), e.Table),
				Fix:     "SET lock_timeout = '2s' before it, and retry the migration if it times out"})
		}
	}
	sort.SliceStable(r.Findings, func(i, j int) bool { return r.Findings[i].Severity > r.Findings[j].Severity })
	for t := range l.created { // including a table this statement created
		r.NewTables[t] = true
	}
	return r
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (l *linter) set(c *sqlparse.Cursor, s *stmt) {
	reset := c.Accept("reset")
	if !reset {
		c.Accept("set")
	}
	local := c.Accept("local")
	c.Accept("session")
	if reset {
		if c.Accept("all") {
			l.session, l.hasLocal = 0, false
		} else if c.Accept("lock_timeout") {
			l.session = 0
		}
		return
	}
	if !c.Accept("lock_timeout") {
		return
	}
	if !c.AcceptPunct("=") {
		c.Accept("to")
	}
	t := c.Next()
	var d time.Duration
	if !t.Is("default") {
		var err error
		d, err = parseTimeout(t.Text)
		if err != nil {
			s.notes = append(s.notes, "could not read lock_timeout value: "+err.Error())
			return
		}
	}
	if local {
		if l.inTx {
			l.local, l.hasLocal = d, true
		}
		return
	}
	l.session = d
}

// parseTimeout reads a lock_timeout value: a bare number is milliseconds.
func parseTimeout(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(n * float64(time.Millisecond)), nil
	}
	num := strings.TrimRight(v, "abcdefghijklmnopqrstuvwxyz ")
	unit := strings.TrimSpace(v[len(num):])
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q", v)
	}
	mult := map[string]time.Duration{"us": time.Microsecond, "ms": time.Millisecond, "s": time.Second,
		"min": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("bad unit in %q", v)
	}
	return time.Duration(n * float64(mult)), nil
}

func (l *linter) create(c *sqlparse.Cursor, s *stmt) {
	c.Accept("or", "replace")
	unique := c.Accept("unique")
	switch {
	case c.Accept("index"):
		s.kind = "CREATE INDEX"
		if unique {
			s.kind = "CREATE UNIQUE INDEX"
		}
		conc := c.Accept("concurrently")
		c.Accept("if", "not", "exists")
		name := ""
		if !c.Peek(0).Is("on") {
			name, _ = c.Name()
		}
		if !c.Accept("on") {
			s.unknown = "could not find the table in CREATE INDEX"
			return
		}
		c.Accept("only")
		t, _ := c.Name()
		if name != "" {
			l.schema.Indexes[name] = t
		}
		if conc {
			s.kind += " CONCURRENTLY"
			s.notx = true
			e := s.lock(t, ShareUpdateExclusive, "CREATE INDEX CONCURRENTLY")
			e.Scan = true
			s.notes = append(s.notes, "builds without blocking reads or writes, but waits for every transaction already running; if it fails it leaves an INVALID index to drop")
			return
		}
		e := s.lock(t, Share, "CREATE INDEX")
		e.Scan = true
		s.fix(t, "CREATE INDEX CONCURRENTLY (outside a transaction block)")
	case c.Accept("table"), c.Accept("unlogged", "table"), c.Accept("temp", "table"), c.Accept("temporary", "table"):
		s.kind = "CREATE TABLE"
		c.Accept("if", "not", "exists")
		t, ok := c.Name()
		if !ok {
			s.unknown = "could not read the table name"
			return
		}
		l.created[t] = true
		tbl := l.schema.Table(t)
		g, ok := c.Group()
		if !ok {
			return // CREATE TABLE ... AS
		}
		for _, item := range sqlparse.SplitTop(g) {
			l.tableElement(item, tbl, s)
		}
	case c.Accept("trigger"), c.Accept("constraint", "trigger"):
		s.kind = "CREATE TRIGGER"
		for !c.Done() && !c.Peek(0).Is("on") {
			c.Next()
		}
		if c.Accept("on") {
			if t, ok := c.Name(); ok {
				s.lock(t, ShareRowExclusive, "CREATE TRIGGER")
			}
		}
	case c.Accept("function"), c.Accept("procedure"):
		s.kind = "CREATE FUNCTION"
	case c.Peek(0).Is("view") || c.Peek(0).Is("sequence") || c.Peek(0).Is("type") ||
		c.Peek(0).Is("extension") || c.Peek(0).Is("schema") || c.Peek(0).Is("materialized"):
		s.kind = "CREATE " + strings.ToUpper(c.Peek(0).Text)
	default:
		s.kind = "CREATE " + strings.ToUpper(c.Peek(0).Text)
		s.unknown = "statement type not analysed"
	}
}

// tableElement handles one column or constraint inside CREATE TABLE (...).
func (l *linter) tableElement(item []sqlparse.Token, tbl *Table, s *stmt) {
	c := &sqlparse.Cursor{T: item}
	if c.Peek(0).Is("constraint") || c.Peek(0).Is("check") || c.Peek(0).Is("foreign") ||
		c.Peek(0).Is("primary") || c.Peek(0).Is("unique") || c.Peek(0).Is("exclude") || c.Peek(0).Is("like") {
		name := ""
		if c.Accept("constraint") {
			name, _ = c.Name()
		}
		switch {
		case c.Accept("foreign", "key"):
			c.Group()
			if c.Accept("references") {
				if ref, ok := c.Name(); ok {
					s.lock(ref, ShareRowExclusive, "a foreign key on a new table locks the referenced table")
					tbl.Constraints[firstNonEmpty(name, "fk_"+ref)] = &Constraint{Kind: "fk", RefTable: ref, Validated: true}
				}
			}
		case c.Accept("check"):
			if g, ok := c.Group(); ok {
				tbl.Constraints[firstNonEmpty(name, "check")] = &Constraint{Kind: "check", NotNullCol: notNullCheck(g), Validated: true}
			}
		case c.Accept("primary", "key"):
			if g, ok := c.Group(); ok {
				for _, col := range sqlparse.SplitTop(g) {
					if len(col) == 1 {
						if cc, ok := tbl.Columns[col[0].Text]; ok {
							cc.NotNull = true
						}
					}
				}
			}
		}
		return
	}
	col, ok := c.Name()
	if !ok {
		return
	}
	typ, cons := splitColumnDef(c)
	cc := &Column{Type: NormType(sqlparse.Join(typ))}
	tbl.Columns[col] = cc
	for i := 0; i < len(cons); i++ {
		switch {
		case cons[i].Is("not") && i+1 < len(cons) && cons[i+1].Is("null"), cons[i].Is("primary"):
			cc.NotNull = true
		case cons[i].Is("references") && i+1 < len(cons) && cons[i+1].Name():
			ref := cons[i+1].Text
			s.lock(ref, ShareRowExclusive, "a foreign key on a new table locks the referenced table")
			tbl.Constraints["fk_"+col] = &Constraint{Kind: "fk", RefTable: ref, Validated: true}
		}
	}
}

// columnConstraintWords end a column's type in a column definition.
var columnConstraintWords = map[string]bool{"default": true, "not": true, "null": true, "constraint": true,
	"primary": true, "unique": true, "references": true, "check": true, "generated": true, "collate": true}

// splitColumnDef consumes "type [constraints...]" and returns both parts.
func splitColumnDef(c *sqlparse.Cursor) (typ, cons []sqlparse.Token) {
	depth := 0
	for !c.Done() {
		t := c.Peek(0)
		if depth == 0 && t.Kind == sqlparse.Word && columnConstraintWords[t.Text] {
			break
		}
		if t.Kind == sqlparse.Punct && t.Text == "(" {
			depth++
		} else if t.Kind == sqlparse.Punct && t.Text == ")" {
			depth--
		}
		typ = append(typ, c.Next())
	}
	cons = c.T[c.I:]
	c.I = len(c.T)
	return typ, cons
}

// notNullCheck returns col when the CHECK expression is exactly "col IS NOT NULL".
func notNullCheck(g []sqlparse.Token) string {
	for len(g) >= 2 && g[0].Kind == sqlparse.Punct && g[0].Text == "(" && g[len(g)-1].Text == ")" {
		g = g[1 : len(g)-1]
	}
	if len(g) == 4 && g[0].Name() && g[1].Is("is") && g[2].Is("not") && g[3].Is("null") {
		return g[0].Text
	}
	return ""
}

func (l *linter) alterTable(c *sqlparse.Cursor, s *stmt) {
	c.Accept("if", "exists")
	c.Accept("only")
	t, ok := c.Name()
	if !ok {
		s.unknown = "could not read the table name"
		return
	}
	c.AcceptPunct("*")
	tbl := l.schema.Table(t)
	if c.Accept("rename", "to") {
		if nt, ok := c.Name(); ok {
			s.lock(t, AccessExclusive, "RENAME TO")
			delete(l.schema.Tables, t)
			tbl.Name = nt
			l.schema.Tables[nt] = tbl
		}
		return
	}
	for _, action := range sqlparse.SplitTop(c.T[c.I:]) {
		l.alterAction(&sqlparse.Cursor{T: action}, t, tbl, s)
		if s.unknown != "" {
			return
		}
	}
}

func (l *linter) alterAction(c *sqlparse.Cursor, t string, tbl *Table, s *stmt) {
	switch {
	case c.Accept("add"):
		if c.Peek(0).Is("constraint") || c.Peek(0).Is("check") || c.Peek(0).Is("foreign") ||
			c.Peek(0).Is("unique") || c.Peek(0).Is("primary") || c.Peek(0).Is("exclude") {
			l.addConstraint(c, t, tbl, s)
			return
		}
		c.Accept("column")
		c.Accept("if", "not", "exists")
		l.addColumn(c, t, tbl, s)
	case c.Accept("drop"):
		switch {
		case c.Accept("constraint"):
			c.Accept("if", "exists")
			name, _ := c.Name()
			s.lock(t, AccessExclusive, "DROP CONSTRAINT")
			if con, ok := tbl.Constraints[name]; ok && con.Kind == "fk" {
				s.lock(con.RefTable, AccessExclusive, "dropping a foreign key also locks the referenced table")
			}
			delete(tbl.Constraints, name)
		default:
			c.Accept("column")
			c.Accept("if", "exists")
			col, _ := c.Name()
			s.lock(t, AccessExclusive, "DROP COLUMN")
			delete(tbl.Columns, col)
			s.notes = append(s.notes, "DROP COLUMN only hides the column; space comes back as rows are rewritten")
		}
	case c.Accept("alter"):
		c.Accept("column")
		col, ok := c.Name()
		if !ok {
			s.unknown = "could not read the column name"
			return
		}
		l.alterColumn(c, t, col, tbl, s)
	case c.Accept("validate", "constraint"):
		name, _ := c.Name()
		e := s.lock(t, ShareUpdateExclusive, "VALIDATE CONSTRAINT")
		e.Scan = true
		con, ok := tbl.Constraints[name]
		if !ok {
			s.notes = append(s.notes, "constraint "+name+" is not in the known schema; assumed to be a CHECK")
			con = &Constraint{Kind: "check"}
			tbl.Constraints[name] = con
		}
		if con.Kind == "fk" {
			re := s.lock(con.RefTable, RowShare, "validating a foreign key reads the referenced table")
			re.Scan = true
		}
		con.Validated = true
	case c.Accept("rename"):
		c.Accept("column")
		c.Accept("constraint")
		old, _ := c.Name()
		c.Accept("to")
		nw, _ := c.Name()
		s.lock(t, AccessExclusive, "RENAME")
		if col, ok := tbl.Columns[old]; ok {
			delete(tbl.Columns, old)
			tbl.Columns[nw] = col
		}
		if con, ok := tbl.Constraints[old]; ok {
			delete(tbl.Constraints, old)
			tbl.Constraints[nw] = con
		}
	case c.Accept("set", "logged"), c.Accept("set", "unlogged"), c.Accept("set", "tablespace"):
		e := s.lock(t, AccessExclusive, "rewrites the table")
		e.Rewrite, e.Scan = true, true
	case c.Peek(0).Is("set") && c.Peek(1).Kind == sqlparse.Punct && c.Peek(1).Text == "(",
		c.Peek(0).Is("reset") && c.Peek(1).Kind == sqlparse.Punct && c.Peek(1).Text == "(":
		s.lock(t, ShareUpdateExclusive, "storage parameters")
	case c.Accept("enable"), c.Accept("disable"):
		if c.Accept("trigger") || c.Accept("replica", "trigger") || c.Accept("always", "trigger") {
			s.lock(t, ShareRowExclusive, "ENABLE/DISABLE TRIGGER")
			return
		}
		s.unknown = "ALTER TABLE ENABLE/DISABLE " + strings.ToUpper(c.Peek(0).Text) + " is not analysed"
	case c.Accept("owner", "to"):
		s.lock(t, AccessExclusive, "OWNER TO")
	default:
		s.unknown = "ALTER TABLE " + strings.ToUpper(c.Peek(0).Text) + " " + strings.ToUpper(c.Peek(1).Text) + " is not analysed"
	}
}

// Volatile functions force ADD COLUMN ... DEFAULT to rewrite the table.
var volatileFuncs = map[string]bool{"random": true, "gen_random_uuid": true, "uuid_generate_v1": true,
	"uuid_generate_v1mc": true, "uuid_generate_v4": true, "uuidv4": true, "uuidv7": true, "clock_timestamp": true,
	"timeofday": true, "nextval": true, "setval": true, "txid_current": true}

// Functions that are immutable or stable and safe as a default without a rewrite.
var stableFuncs = map[string]bool{"now": true, "transaction_timestamp": true, "statement_timestamp": true,
	"lower": true, "upper": true, "coalesce": true, "length": true, "abs": true, "round": true,
	"to_timestamp": true, "make_interval": true, "jsonb_build_object": true, "json_build_object": true,
	"array": true, "nullif": true, "greatest": true, "least": true, "concat": true}

// defaultVolatility classifies a DEFAULT expression.
func defaultVolatility(expr []sqlparse.Token) (volatile bool, why string) {
	for i, t := range expr {
		if t.Kind != sqlparse.Word || i+1 >= len(expr) || expr[i+1].Kind != sqlparse.Punct || expr[i+1].Text != "(" {
			continue
		}
		if i > 0 && expr[i-1].Text == "::" {
			continue // a type modifier such as ::numeric(10,2)
		}
		switch {
		case volatileFuncs[t.Text]:
			return true, t.Text + "() is volatile, so every existing row needs its own value"
		case stableFuncs[t.Text]:
		default:
			return true, "the volatility of " + t.Text + "() is unknown, so a rewrite is assumed"
		}
	}
	return false, ""
}

func (l *linter) addColumn(c *sqlparse.Cursor, t string, tbl *Table, s *stmt) {
	col, ok := c.Name()
	if !ok {
		s.unknown = "could not read the column name"
		return
	}
	typTok, cons := splitColumnDef(c)
	typRaw := strings.ToLower(sqlparse.Join(typTok))
	cc := &Column{Type: NormType(typRaw)}
	tbl.Columns[col] = cc
	e := s.lock(t, AccessExclusive, "ADD COLUMN")
	rewrite, why := false, ""
	if strings.Contains(typRaw, "serial") {
		rewrite, why = true, "a serial column fills every existing row from a sequence"
	}
	hasDefault, notNull := false, false
	for i := 0; i < len(cons); i++ {
		w := cons[i]
		switch {
		case w.Is("default"):
			hasDefault = true
			j := i + 1
			for j < len(cons) && !(cons[j].Kind == sqlparse.Word && columnConstraintWords[cons[j].Text] && cons[j].Text != "null") {
				j++
			}
			expr := cons[i+1 : j]
			isNull := len(expr) == 1 && expr[0].Is("null")
			if v, w := defaultVolatility(expr); v && !rewrite {
				rewrite, why = true, w
			} else if !isNull && l.pg < 11 && !rewrite {
				rewrite, why = true, "before PostgreSQL 11, any non-null default rewrites the table"
			}
			i = j - 1
		case w.Is("not") && i+1 < len(cons) && cons[i+1].Is("null"):
			notNull = true
			cc.NotNull = true
		case w.Is("generated"):
			if i+3 < len(cons) && cons[i+3].Is("identity") || i+4 < len(cons) && cons[i+4].Is("identity") {
				rewrite, why = true, "an identity column fills every existing row"
				cc.NotNull = true
			} else {
				rewrite, why = true, "a stored generated column is computed for every existing row"
			}
		case w.Is("primary") || w.Is("unique"):
			e.Scan = true
			e.Why = append(e.Why, "builds an index")
			if w.Is("primary") {
				cc.NotNull = true
			}
		case w.Is("references") && i+1 < len(cons) && cons[i+1].Name():
			ref := cons[i+1].Text
			s.lock(t, ShareRowExclusive, "")
			re := s.lock(ref, ShareRowExclusive, "adding a foreign key locks the referenced table")
			re.Scan, e.Scan = true, true
			tbl.Constraints["fk_"+col] = &Constraint{Kind: "fk", RefTable: ref, Validated: true}
		case w.Is("check"):
			e.Scan = true
			e.Why = append(e.Why, "validates a CHECK constraint")
		}
	}
	if notNull && !hasDefault && !rewrite {
		s.notes = append(s.notes, "NOT NULL with no DEFAULT fails if "+t+" has any rows")
	}
	if rewrite {
		e.Rewrite, e.Scan = true, true
		e.Why = append(e.Why, why)
		s.fix(t, "add the column without the default, SET DEFAULT for new rows, then backfill existing rows in batches")
	}
}

func (l *linter) alterColumn(c *sqlparse.Cursor, t, col string, tbl *Table, s *stmt) {
	cc := tbl.Columns[col]
	switch {
	case c.Accept("set", "data", "type"), c.Accept("type"):
		var typTok []sqlparse.Token
		using := false
		for !c.Done() {
			if c.Peek(0).Is("using") {
				using = true
				break
			}
			if c.Peek(0).Is("collate") {
				break
			}
			typTok = append(typTok, c.Next())
		}
		nt := NormType(sqlparse.Join(typTok))
		old := ""
		if cc != nil {
			old = cc.Type
		}
		rewrite, why := rewriteForType(old, nt, using)
		e := s.lock(t, AccessExclusive, "ALTER COLUMN TYPE")
		if rewrite {
			e.Rewrite, e.Scan = true, true
			e.Why = append(e.Why, why)
			s.fix(t, "add a new column of the new type, keep it in sync with a trigger, backfill it in batches, then swap the columns in one short transaction")
		} else {
			s.notes = append(s.notes, why)
		}
		if cc != nil {
			cc.Type = nt
		} else {
			tbl.Columns[col] = &Column{Type: nt}
		}
	case c.Accept("set", "not", "null"):
		e := s.lock(t, AccessExclusive, "SET NOT NULL")
		if l.pg >= 12 && tbl.hasValidNotNullCheck(col) {
			s.notes = append(s.notes, "a validated CHECK ("+col+" IS NOT NULL) lets PostgreSQL skip the scan")
		} else {
			e.Scan = true
			e.Why = append(e.Why, "checks every row for NULLs")
			if l.pg >= 12 {
				s.fix(t, "ADD CONSTRAINT ... CHECK ("+col+" IS NOT NULL) NOT VALID, then VALIDATE CONSTRAINT, then SET NOT NULL (no scan), then drop the check")
			}
		}
		if cc != nil {
			cc.NotNull = true
		}
	case c.Accept("drop", "not", "null"):
		s.lock(t, AccessExclusive, "DROP NOT NULL")
		if cc != nil {
			cc.NotNull = false
		}
	case c.Accept("set", "default"), c.Accept("drop", "default"):
		s.lock(t, AccessExclusive, "column default")
	case c.Accept("set", "statistics"):
		s.lock(t, ShareUpdateExclusive, "SET STATISTICS")
	case c.Accept("set", "storage"), c.Accept("set", "compression"):
		s.lock(t, AccessExclusive, "column storage")
	default:
		s.unknown = "ALTER COLUMN " + strings.ToUpper(c.Peek(0).Text) + " " + strings.ToUpper(c.Peek(1).Text) + " is not analysed"
	}
}

func (l *linter) addConstraint(c *sqlparse.Cursor, t string, tbl *Table, s *stmt) {
	name := ""
	if c.Accept("constraint") {
		name, _ = c.Name()
	}
	notValid := false
	for k := c.I; k+1 < len(c.T); k++ {
		if c.T[k].Is("not") && c.T[k+1].Is("valid") {
			notValid = true
		}
	}
	switch {
	case c.Accept("check"):
		g, _ := c.Group()
		e := s.lock(t, AccessExclusive, "ADD CHECK")
		if notValid {
			s.notes = append(s.notes, "NOT VALID skips checking existing rows; VALIDATE CONSTRAINT later takes only SHARE UPDATE EXCLUSIVE")
		} else {
			e.Scan = true
			e.Why = append(e.Why, "checks every row")
			s.fix(t, "add it NOT VALID, then VALIDATE CONSTRAINT in a separate statement")
		}
		tbl.Constraints[firstNonEmpty(name, "check")] = &Constraint{Kind: "check", NotNullCol: notNullCheck(g), Validated: !notValid}
	case c.Accept("foreign", "key"):
		c.Group()
		if !c.Accept("references") {
			s.unknown = "could not read REFERENCES"
			return
		}
		ref, _ := c.Name()
		e := s.lock(t, ShareRowExclusive, "ADD FOREIGN KEY")
		re := s.lock(ref, ShareRowExclusive, "adding a foreign key locks the referenced table")
		if notValid {
			s.notes = append(s.notes, "NOT VALID skips checking existing rows; VALIDATE CONSTRAINT later takes only SHARE UPDATE EXCLUSIVE")
		} else {
			e.Scan, re.Scan = true, true
			e.Why = append(e.Why, "checks every row against "+ref)
			s.fix(t, "add it NOT VALID, then VALIDATE CONSTRAINT in a separate statement")
			s.fix(ref, "add it NOT VALID, then VALIDATE CONSTRAINT in a separate statement")
		}
		tbl.Constraints[firstNonEmpty(name, "fk_"+ref)] = &Constraint{Kind: "fk", RefTable: ref, Validated: !notValid}
	case c.Accept("unique"), c.Accept("primary", "key"):
		pk := c.T[c.I-1].Is("key")
		e := s.lock(t, AccessExclusive, "ADD UNIQUE/PRIMARY KEY")
		if c.Accept("using", "index") {
			idx, _ := c.Name()
			s.notes = append(s.notes, "USING INDEX "+idx+" reuses an existing index, so nothing is built")
		} else {
			e.Scan = true
			e.Why = append(e.Why, "builds a unique index")
			s.fix(t, "CREATE UNIQUE INDEX CONCURRENTLY first, then ADD CONSTRAINT ... USING INDEX")
			if g, ok := c.Group(); ok && pk {
				for _, col := range sqlparse.SplitTop(g) {
					if len(col) == 1 {
						if cc, ok := tbl.Columns[col[0].Text]; ok {
							cc.NotNull = true
						}
					}
				}
			}
		}
		kind := "unique"
		if pk {
			kind = "pk"
		}
		tbl.Constraints[firstNonEmpty(name, kind)] = &Constraint{Kind: kind, Validated: true}
	case c.Accept("exclude"):
		e := s.lock(t, AccessExclusive, "ADD EXCLUDE")
		e.Scan = true
	default:
		s.unknown = "constraint type not analysed"
	}
}

func (l *linter) drop(c *sqlparse.Cursor, s *stmt) {
	switch {
	case c.Accept("index"):
		s.kind = "DROP INDEX"
		conc := c.Accept("concurrently")
		c.Accept("if", "exists")
		for {
			name, ok := c.Name()
			if !ok {
				break
			}
			t, known := l.schema.Indexes[name]
			if !known {
				s.notes = append(s.notes, "index "+name+" is not in the known schema, so its table is unknown")
			} else if conc {
				s.lock(t, ShareUpdateExclusive, "DROP INDEX CONCURRENTLY")
			} else {
				s.lock(t, AccessExclusive, "DROP INDEX")
				s.fix(t, "DROP INDEX CONCURRENTLY (outside a transaction block)")
			}
			delete(l.schema.Indexes, name)
			if !c.AcceptPunct(",") {
				break
			}
		}
		if conc {
			s.kind += " CONCURRENTLY"
			s.notx = true
		}
	case c.Accept("table"):
		s.kind = "DROP TABLE"
		c.Accept("if", "exists")
		for {
			t, ok := c.Name()
			if !ok {
				break
			}
			s.lock(t, AccessExclusive, "DROP TABLE")
			if tbl, ok := l.schema.Tables[t]; ok {
				for _, con := range tbl.Constraints {
					if con.Kind == "fk" && con.RefTable != t {
						s.lock(con.RefTable, AccessExclusive, "dropping a table removes its foreign key's triggers from "+con.RefTable)
					}
				}
			}
			delete(l.schema.Tables, t)
			if !c.AcceptPunct(",") {
				break
			}
		}
	case c.Accept("trigger"):
		s.kind = "DROP TRIGGER"
		c.Accept("if", "exists")
		c.Name()
		if c.Accept("on") {
			if t, ok := c.Name(); ok {
				s.lock(t, AccessExclusive, "DROP TRIGGER")
			}
		}
	case c.Accept("function"), c.Accept("procedure"), c.Accept("view"), c.Accept("sequence"), c.Accept("type"):
		s.kind = "DROP " + strings.ToUpper(c.T[c.I-1].Text)
	default:
		s.kind = "DROP " + strings.ToUpper(c.Peek(0).Text)
		s.unknown = "statement type not analysed"
	}
}

func (l *linter) dml(c *sqlparse.Cursor, s *stmt, batch *Batch) {
	c.Accept("only")
	t, ok := c.Name()
	if !ok {
		s.unknown = "could not read the table name"
		return
	}
	e := s.lock(t, RowExclusive, s.kind)
	e.ScanKnown = false
	hasWhere := false
	depth := 0
	usesRange := false
	for _, tok := range c.T[c.I:] {
		if tok.Kind == sqlparse.Punct && tok.Text == "(" {
			depth++
		} else if tok.Kind == sqlparse.Punct && tok.Text == ")" {
			depth--
		} else if depth == 0 && tok.Is("where") {
			hasWhere = true
		}
		if tok.Is("lo") || tok.Is("hi") {
			usesRange = true
		}
	}
	switch {
	case batch != nil:
		if !usesRange {
			s.unknown = "a batch statement must use :lo and :hi in its WHERE clause"
			return
		}
		e.AllRows = false
	case hasWhere:
		e.AllRowsKnown = false
		s.notes = append(s.notes, "row-locks every row it matches until it commits; if that is most of "+t+", batch it")
	default:
		e.AllRows = true
		e.Why = append(e.Why, "no WHERE clause")
	}
}

func (l *linter) lockTable(c *sqlparse.Cursor, s *stmt) {
	c.Accept("table")
	c.Accept("only")
	var tables []string
	for {
		t, ok := c.Name()
		if !ok {
			break
		}
		tables = append(tables, t)
		if !c.AcceptPunct(",") {
			break
		}
	}
	mode := AccessExclusive
	if c.Accept("in") {
		var words []string
		for !c.Done() && !c.Peek(0).Is("mode") {
			words = append(words, c.Next().Text)
		}
		m, err := ParseMode(strings.Join(words, " "))
		if err != nil {
			s.unknown = err.Error()
			return
		}
		mode = m
	}
	for _, t := range tables {
		s.lock(t, mode, "LOCK TABLE")
	}
	if !l.inTx {
		s.notes = append(s.notes, "LOCK TABLE outside a transaction block is an error")
	}
}

func (l *linter) reindex(c *sqlparse.Cursor, s *stmt) {
	c.Group()
	switch {
	case c.Accept("index"):
		conc := c.Accept("concurrently")
		name, _ := c.Name()
		t, ok := l.schema.Indexes[name]
		if !ok {
			s.notes = append(s.notes, "index "+name+" is not in the known schema, so its table is unknown")
			return
		}
		l.reindexTable(t, conc, s)
	case c.Accept("table"):
		conc := c.Accept("concurrently")
		t, _ := c.Name()
		l.reindexTable(t, conc, s)
	default:
		s.unknown = "REINDEX " + strings.ToUpper(c.Peek(0).Text) + " is not analysed"
	}
}

func (l *linter) reindexTable(t string, conc bool, s *stmt) {
	if conc {
		s.kind += " CONCURRENTLY"
		s.notx = true
		s.lock(t, ShareUpdateExclusive, "REINDEX CONCURRENTLY").Scan = true
		return
	}
	s.lock(t, Share, "REINDEX").Scan = true
	s.fix(t, "REINDEX ... CONCURRENTLY (PostgreSQL 12+)")
}
