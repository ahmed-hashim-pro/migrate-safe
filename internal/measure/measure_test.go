package measure

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestParseWorkload(t *testing.T) {
	w, err := ParseWorkload(`-- comment
-- param: id = orders.id
-- think: 5ms
-- class: read workers=3
SELECT * FROM orders
WHERE id = :id;
-- class: write
UPDATE orders SET x = 1 WHERE id = :id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Classes) != 2 || w.Classes[0].Workers != 3 || w.Classes[1].Workers != 1 {
		t.Fatalf("classes: %+v", w.Classes)
	}
	if w.Classes[0].SQL != "SELECT * FROM orders\nWHERE id = :id" {
		t.Errorf("SQL = %q", w.Classes[0].SQL)
	}
	if w.Think != 5*time.Millisecond || len(w.Params) != 1 || w.Params[0].Table != "orders" {
		t.Errorf("think %s, params %+v", w.Think, w.Params)
	}
	if err := w.SetWorkers("read=8, write=0"); err != nil || w.Workers() != 8 {
		t.Errorf("SetWorkers: %v, total %d", err, w.Workers())
	}
	if err := w.SetWorkers("nosuch=1"); err == nil {
		t.Error("SetWorkers accepted an unknown class")
	}
	for _, bad := range []string{"SELECT 1;", "-- class: read\n", "-- class: read speed=2\nSELECT 1", "-- param: id orders.id\n-- class: r\nSELECT 1"} {
		if _, err := ParseWorkload(bad); err == nil {
			t.Errorf("ParseWorkload(%q) succeeded", bad)
		}
	}
}

func TestRender(t *testing.T) {
	w := &Workload{Params: []Param{{Name: "id", lo: 10, hi: 12}}}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 50; i++ {
		got := w.render("SELECT :id, x::text, ':id' FROM t WHERE id=:id AND y = :other", rng)
		if strings.Contains(got, "x::text") == false || !strings.Contains(got, ":other") {
			t.Fatalf("render broke a cast or an unknown param: %s", got)
		}
		f := strings.Fields(got)
		if v := f[1]; v != "10," && v != "11," && v != "12," {
			t.Fatalf("value out of range: %s", got)
		}
	}
}

func TestBlockedIn(t *testing.T) {
	ms := time.Millisecond
	s := []ProbeSample{
		{At: 0, Table: "t", Blocked: false},
		{At: 10 * ms, Table: "t", Blocked: true},
		{At: 20 * ms, Table: "t", Blocked: true},
		{At: 30 * ms, Table: "t", Blocked: false},
		{At: 30 * ms, Table: "t", Writes: true, Blocked: true},
		{At: 40 * ms, Table: "u", Blocked: true},
		{At: 50 * ms, Table: "t", Blocked: true},
	}
	if got := blockedIn(s, "t", false, 0, 100*ms); got != 20*ms+50*ms {
		t.Errorf("reads blocked %s, want 70ms (20ms, then from 50ms to the end of the window)", got)
	}
	if got := blockedIn(s, "t", false, 15*ms, 25*ms); got != 10*ms {
		t.Errorf("clipped window: %s, want 10ms", got)
	}
	if got := blockedIn(s, "t", true, 0, 60*ms); got != 30*ms {
		t.Errorf("writes blocked %s, want 30ms", got)
	}
}

func TestClassStats(t *testing.T) {
	ms := time.Millisecond
	qs := []Query{
		{Class: 0, Start: 0, End: 2 * ms},
		{Class: 0, Start: 5 * ms, End: 900 * ms}, // overlaps the window
		{Class: 1, Start: 100 * ms, End: 101 * ms, Err: true},
		{Class: 1, Start: 2000 * ms, End: 2001 * ms}, // after it
	}
	st := classStats(qs, []string{"read", "write"}, 10*ms, 1000*ms, false)
	if st["read"].Count != 1 || st["read"].Max != 895*ms {
		t.Errorf("read: %+v", st["read"])
	}
	if st["write"].Count != 1 || st["write"].Errors != 1 {
		t.Errorf("write: %+v", st["write"])
	}
	within := classStats(qs, []string{"read", "write"}, 0, 1000*ms, true)
	if within["read"].Count != 2 {
		t.Errorf("within: %+v", within["read"])
	}
}

func TestNotNullCol(t *testing.T) {
	for def, want := range map[string]string{
		"CHECK ((status IS NOT NULL))":           "status",
		"CHECK ((status IS NOT NULL)) NOT VALID": "status",
		`CHECK (("Status" IS NOT NULL))`:         "Status",
		"CHECK ((amount > 0))":                   "",
		"CHECK (((a IS NOT NULL) AND (b > 0)))":  "",
		"FOREIGN KEY (a) REFERENCES b(id)":       "",
	} {
		if got := notNullCol(def); got != want {
			t.Errorf("notNullCol(%q) = %q, want %q", def, got, want)
		}
	}
}
