package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func chdirRepo(t *testing.T) {
	t.Helper()
	wd, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
}

func TestLintExitCodes(t *testing.T) {
	chdirRepo(t)
	for _, dir := range []string{"01-create-index", "02-add-foreign-key", "03-set-not-null", "04-add-column-volatile-default", "05-change-column-type", "06-lock-queue"} {
		code, out, errOut := runCmd(t, "lint", "--schema", "examples/schema.sql", filepath.Join("examples/migrations", dir, "unsafe.sql"))
		if code != 1 || !(strings.Contains(out, "DANGER") || strings.Contains(out, "RISK")) {
			t.Errorf("%s/unsafe: exit %d\n%s%s", dir, code, out, errOut)
		}
		code, out, errOut = runCmd(t, "lint", "--schema", "examples/schema.sql", filepath.Join("examples/migrations", dir, "safe.sql"))
		if code != 0 || !strings.Contains(out, "nothing found") {
			t.Errorf("%s/safe: exit %d\n%s%s", dir, code, out, errOut)
		}
	}
	// --fail-on danger lets a risk-only migration pass.
	if code, _, _ := runCmd(t, "lint", "--fail-on", "danger", "examples/migrations/06-lock-queue/unsafe.sql"); code != 0 {
		t.Errorf("--fail-on danger: exit %d", code)
	}
	if code, _, _ := runCmd(t, "lint", "--fail-on", "never", "examples/migrations/01-create-index/unsafe.sql"); code != 0 {
		t.Errorf("--fail-on never: exit %d", code)
	}
}

func TestLintFlagsAfterFiles(t *testing.T) {
	chdirRepo(t)
	code, out, _ := runCmd(t, "lint", "examples/migrations/03-set-not-null/unsafe.sql", "--pg", "11", "-v")
	if code != 1 || !strings.Contains(out, "PostgreSQL 11") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestLintJSON(t *testing.T) {
	chdirRepo(t)
	code, out, _ := runCmd(t, "lint", "--json", "--schema", "examples/schema.sql", "examples/migrations/02-add-foreign-key/safe.sql")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got []jsonLint
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].Kind != "ALTER TABLE" || got[2].Held["orders"] != "SHARE UPDATE EXCLUSIVE" || got[2].Held["accounts"] != "ROW SHARE" {
		t.Errorf("unexpected JSON: %s", out)
	}
	if got[2].LockTimeout != "2s" || got[2].Effects[0].Scan == nil || !*got[2].Effects[0].Scan {
		t.Errorf("unexpected JSON: %s", out)
	}
}

func TestUsageAndErrors(t *testing.T) {
	if code, _, errOut := runCmd(t); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("no args: exit %d", code)
	}
	if code, _, _ := runCmd(t, "frobnicate"); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
	if code, _, errOut := runCmd(t, "lint"); code != 2 || !strings.Contains(errOut, "no migration files") {
		t.Errorf("lint without files: exit %d %s", code, errOut)
	}
	if code, _, errOut := runCmd(t, "lint", "/nonexistent.sql"); code != 2 || !strings.Contains(errOut, "no such file") {
		t.Errorf("missing file: exit %d %s", code, errOut)
	}
	if code, _, errOut := runCmd(t, "measure", "x.sql"); code != 2 || !strings.Contains(errOut, "--dsn is required") {
		t.Errorf("measure without dsn: exit %d %s", code, errOut)
	}
	if code, _, _ := runCmd(t, "lint", "-h"); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
}

func TestWithDatabase(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://postgres:pg@localhost:5432/postgres": "postgres://postgres:pg@localhost:5432/demo",
		"postgresql://u@h/postgres?sslmode=disable":      "postgresql://u@h/demo?sslmode=disable",
		"postgres://postgres:pg@localhost:5432":          "postgres://postgres:pg@localhost:5432/demo",
		"host=localhost user=postgres dbname=postgres":   "host=localhost user=postgres dbname=postgres dbname=demo",
	} {
		got, err := withDatabase(in, "demo")
		if err != nil || got != want {
			t.Errorf("withDatabase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
