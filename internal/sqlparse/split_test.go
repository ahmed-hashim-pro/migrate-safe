package sqlparse

import (
	"strings"
	"testing"
)

func TestSplitBoundaries(t *testing.T) {
	src := `-- leading comment; not a statement
SELECT 'a;b', "we;ird", E'it\'s;';
/* block ; /* nested ; */ still comment */
CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1; $$;
CREATE FUNCTION g() RETURNS int LANGUAGE plpgsql AS $body$
BEGIN
  RETURN 2; -- $$ inside a tagged body
END
$body$;
SELECT $1::int;
UPDATE t SET x = 1
WHERE y = 'it''s'`
	stmts, err := Split(src)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []int{2, 4, 5, 10, 11}
	if len(stmts) != len(wantLines) {
		for _, s := range stmts {
			t.Logf("%d: %q", s.Line, s.SQL)
		}
		t.Fatalf("got %d statements, want %d", len(stmts), len(wantLines))
	}
	for i, s := range stmts {
		if s.Line != wantLines[i] {
			t.Errorf("statement %d starts on line %d, want %d: %q", i, s.Line, wantLines[i], s.SQL)
		}
	}
	if !strings.Contains(stmts[2].SQL, "RETURN 2;") {
		t.Errorf("dollar-quoted body was split: %q", stmts[2].SQL)
	}
	if strings.HasSuffix(stmts[4].SQL, ";") {
		t.Errorf("trailing semicolon kept: %q", stmts[4].SQL)
	}
}

func TestDirectives(t *testing.T) {
	src := `SET lock_timeout = '1s';

-- migrate-safe: long-reader orders 8s
-- an ordinary comment in between
-- migrate-safe: expect-error 55P03
ALTER TABLE orders ADD COLUMN x int;
SELECT 1;`
	stmts, err := Split(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts[0].Directives) != 0 {
		t.Errorf("first statement got directives %v", stmts[0].Directives)
	}
	if v, _ := stmts[1].Directive("long-reader"); v != "orders 8s" {
		t.Errorf("long-reader = %q", v)
	}
	if v, _ := stmts[1].Directive("expect-error"); v != "55P03" {
		t.Errorf("expect-error = %q", v)
	}
	if len(stmts[2].Directives) != 0 {
		t.Errorf("directives leaked to the next statement: %v", stmts[2].Directives)
	}
}

func TestSplitErrors(t *testing.T) {
	for _, src := range []string{
		"SELECT 'unterminated",
		"SELECT $$ never closed",
		"/* open comment",
		"SELECT 1;\n-- migrate-safe: batch orders.id 10\n",
		"SELECT\n-- migrate-safe: batch orders.id 10\n1;",
	} {
		if _, err := Split(src); err == nil {
			t.Errorf("Split(%q) succeeded, want an error", src)
		}
	}
}

func TestTokenize(t *testing.T) {
	toks := Tokenize(`ALTER TABLE "Orders" ADD COLUMN x numeric(10,2) DEFAULT 'a''b'::text -- gone
  /* gone */ WHERE y = $1 AND z = :lo`)
	var got []string
	for _, tk := range toks {
		got = append(got, tk.Text)
	}
	want := "alter table Orders add column x numeric ( 10 , 2 ) default a'b :: text where y = $1 and z = : lo"
	if strings.Join(got, " ") != want {
		t.Errorf("tokens:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if toks[2].Kind != Ident {
		t.Errorf("quoted identifier kind = %v", toks[2].Kind)
	}
}

func TestCursorHelpers(t *testing.T) {
	c := NewCursor(`public.orders (a, f(b, c), d) rest`)
	name, ok := c.Name()
	if !ok || name != "orders" {
		t.Fatalf("Name() = %q, %v", name, ok)
	}
	g, ok := c.Group()
	if !ok {
		t.Fatal("Group failed")
	}
	if parts := SplitTop(g); len(parts) != 3 {
		t.Errorf("SplitTop gave %d parts, want 3", len(parts))
	}
	if !c.Accept("rest") || !c.Done() {
		t.Error("cursor not at the end")
	}
}
