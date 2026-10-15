package measure

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ahmed-hashim-pro/migrate-safe/internal/lint"
)

// LoadSchema reads the tables, columns, constraints and indexes of every
// user schema into the form the linter uses.
func LoadSchema(ctx context.Context, conn *pgx.Conn) (*lint.Schema, error) {
	s := lint.NewSchema()
	rows, err := conn.Query(ctx, `
SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull
FROM pg_class c
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind IN ('r', 'p')
  AND c.relnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)
  AND c.relnamespace::regnamespace::text NOT LIKE 'pg_toast%'
ORDER BY c.relname, a.attnum`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel, col, typ string
		var nn bool
		if err := rows.Scan(&rel, &col, &typ, &nn); err != nil {
			return nil, err
		}
		s.Table(rel).Columns[col] = &lint.Column{Type: lint.NormType(typ), NotNull: nn}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = conn.Query(ctx, `
SELECT c.relname, k.conname, k.contype::text, coalesce(r.relname, ''), k.convalidated, pg_get_constraintdef(k.oid)
FROM pg_constraint k
JOIN pg_class c ON c.oid = k.conrelid
LEFT JOIN pg_class r ON r.oid = k.confrelid
WHERE c.relnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel, name, typ, ref, def string
		var valid bool
		if err := rows.Scan(&rel, &name, &typ, &ref, &valid, &def); err != nil {
			return nil, err
		}
		con := &lint.Constraint{Validated: valid}
		switch typ {
		case "c":
			con.Kind = "check"
			con.NotNullCol = notNullCol(def)
		case "f":
			con.Kind, con.RefTable = "fk", ref
		case "u":
			con.Kind = "unique"
		case "p":
			con.Kind = "pk"
		default:
			continue
		}
		s.Table(rel).Constraints[name] = con
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = conn.Query(ctx, `
SELECT i.relname, t.relname
FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid JOIN pg_class t ON t.oid = x.indrelid
WHERE t.relnamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)
  AND t.relnamespace::regnamespace::text NOT LIKE 'pg_toast%'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var idx, tbl string
		if err := rows.Scan(&idx, &tbl); err != nil {
			return nil, err
		}
		s.Indexes[idx] = tbl
	}
	return s, rows.Err()
}

// notNullCol recognises pg_get_constraintdef's "CHECK ((col IS NOT NULL))".
func notNullCol(def string) string {
	d := strings.TrimSpace(def)
	if !strings.HasPrefix(d, "CHECK") {
		return ""
	}
	d = strings.TrimSpace(strings.TrimPrefix(d, "CHECK"))
	d = strings.TrimSuffix(d, " NOT VALID")
	for strings.HasPrefix(d, "(") && strings.HasSuffix(d, ")") {
		d = strings.TrimSpace(d[1 : len(d)-1])
	}
	col, ok := strings.CutSuffix(d, " IS NOT NULL")
	if !ok || strings.ContainsAny(col, " ()") {
		return ""
	}
	return strings.Trim(col, `"`)
}
