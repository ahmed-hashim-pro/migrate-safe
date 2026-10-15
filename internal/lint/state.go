package lint

import (
	"regexp"
	"strconv"
	"strings"
)

// Schema is what the linter knows about the database before the migration:
// column types, NOT NULL flags, constraints and indexes. It can come from a
// schema file (ApplySchema) or from a live database's catalog.
type Schema struct {
	Tables  map[string]*Table
	Indexes map[string]string // index name -> table
}

// Table is one table's known shape.
type Table struct {
	Name        string
	Columns     map[string]*Column
	Constraints map[string]*Constraint
}

// Column is one column.
type Column struct {
	Type    string // normalised, see NormType
	NotNull bool
}

// Constraint is a CHECK or FOREIGN KEY constraint the linter tracks.
type Constraint struct {
	Kind       string // "check", "fk", "unique", "pk"
	NotNullCol string // for CHECK (col IS NOT NULL)
	RefTable   string // for fk
	Validated  bool
}

// NewSchema returns an empty schema.
func NewSchema() *Schema {
	return &Schema{Tables: map[string]*Table{}, Indexes: map[string]string{}}
}

// Table returns the named table, creating an empty entry if unknown.
func (s *Schema) Table(name string) *Table {
	t, ok := s.Tables[name]
	if !ok {
		t = &Table{Name: name, Columns: map[string]*Column{}, Constraints: map[string]*Constraint{}}
		s.Tables[name] = t
	}
	return t
}

// Known reports whether the table was declared in the schema.
func (s *Schema) Known(name string) bool {
	_, ok := s.Tables[name]
	return ok
}

// hasValidNotNullCheck reports whether a validated CHECK (col IS NOT NULL)
// exists, which lets SET NOT NULL skip its scan on PostgreSQL 12+.
func (t *Table) hasValidNotNullCheck(col string) bool {
	for _, c := range t.Constraints {
		if c.Kind == "check" && c.NotNullCol == col && c.Validated {
			return true
		}
	}
	return false
}

func (s *Schema) clone() *Schema {
	n := NewSchema()
	for k, v := range s.Indexes {
		n.Indexes[k] = v
	}
	for name, t := range s.Tables {
		nt := n.Table(name)
		for c, col := range t.Columns {
			cc := *col
			nt.Columns[c] = &cc
		}
		for c, con := range t.Constraints {
			cc := *con
			nt.Constraints[c] = &cc
		}
	}
	return n
}

var typeAliases = map[string]string{
	"int": "integer", "int4": "integer", "int8": "bigint", "int2": "smallint",
	"bool": "boolean", "float8": "double precision", "float4": "real",
	"timestamptz": "timestamp with time zone", "timestamp": "timestamp without time zone",
	"timetz": "time with time zone", "time": "time without time zone",
	"varchar": "character varying", "char": "character", "decimal": "numeric",
	"serial": "integer", "bigserial": "bigint", "smallserial": "smallint",
	"serial4": "integer", "serial8": "bigint",
}

var spaces = regexp.MustCompile(`\s+`)

// NormType turns a type as written (or as format_type prints it) into one
// spelling: "varchar(100)" and "character varying(100)" both become
// "character varying(100)".
func NormType(s string) string {
	s = strings.ToLower(strings.TrimSpace(spaces.ReplaceAllString(s, " ")))
	s = strings.ReplaceAll(s, " (", "(")
	if strings.HasPrefix(s, "timestamp") || strings.HasPrefix(s, "time") {
		// The precision in "timestamp(3) with time zone" doesn't matter here.
		if i := strings.Index(s, "("); i >= 0 {
			if j := strings.Index(s[i:], ")"); j >= 0 {
				s = s[:i] + s[i+j+1:]
			}
		}
		s = strings.TrimSpace(s)
	}
	base, mod, _ := strings.Cut(s, "(")
	base = strings.TrimSpace(base)
	if a, ok := typeAliases[base]; ok {
		base = a
	}
	if base == "timestamp with time zone" || base == "timestamp without time zone" {
		// "timestamp(3) with time zone" keeps its precision out of the way.
		return base
	}
	if mod != "" {
		return base + "(" + strings.ReplaceAll(mod, " ", "")
	}
	return base
}

// typeArgs splits "numeric(10,2)" into ("numeric", [10 2]).
func typeArgs(t string) (string, []int) {
	base, mod, ok := strings.Cut(t, "(")
	if !ok {
		return t, nil
	}
	var args []int
	for _, a := range strings.Split(strings.TrimSuffix(mod, ")"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(a))
		if err != nil {
			return base, nil
		}
		args = append(args, n)
	}
	return base, args
}

// rewriteForType decides whether changing a column from old to new type
// rewrites the table. ok is false when old is unknown.
func rewriteForType(old, new string, using bool) (rewrite bool, why string) {
	if using {
		return true, "a USING clause always rewrites the table"
	}
	if old == "" {
		return true, "the column's current type is unknown, so a rewrite is assumed (pass --schema or --dsn)"
	}
	if old == new {
		return false, "the type does not change"
	}
	ob, oa := typeArgs(old)
	nb, na := typeArgs(new)
	switch {
	case ob == "character varying" && nb == "character varying":
		if len(na) == 0 {
			return false, "removing a varchar length limit is binary-compatible"
		}
		if len(oa) == 1 && len(na) == 1 && na[0] >= oa[0] {
			return false, "raising a varchar length limit is binary-compatible"
		}
		return true, "lowering a varchar length limit rewrites the table"
	case (ob == "character varying" || ob == "text") && nb == "text":
		return false, "varchar to text is binary-compatible"
	case ob == "text" && nb == "character varying" && len(na) == 0:
		return false, "text to unbounded varchar is binary-compatible"
	case ob == "numeric" && nb == "numeric":
		if len(na) == 0 {
			return false, "removing numeric precision is binary-compatible"
		}
		if len(oa) == 2 && len(na) == 2 && na[1] == oa[1] && na[0] >= oa[0] {
			return false, "raising numeric precision at the same scale is binary-compatible"
		}
		return true, "changing numeric scale or lowering precision rewrites the table"
	case ob == "timestamp without time zone" && nb == "timestamp with time zone",
		ob == "timestamp with time zone" && nb == "timestamp without time zone":
		return true, "rewrites unless the session TimeZone is UTC (PostgreSQL 12+), which the linter cannot see"
	}
	return true, "changing " + old + " to " + new + " rewrites the table and its indexes"
}
