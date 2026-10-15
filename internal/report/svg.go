package report

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Pair is one example's two measured variants for the chart.
type Pair struct {
	Name         string
	Unsafe, Safe time.Duration // longest stall of any workload query
	UnsafeWhat   string        // e.g. "writes", "reads and writes"
	SafeWhat     string
}

const (
	inkC    = "#24292f"
	mutedC  = "#57606a"
	gridC   = "#d8dee4"
	unsafeC = "#eb6834"
	safeC   = "#2a78d6"
	fontF   = "-apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"
)

// Chart draws the longest query stall per example, as written versus the
// safe rewrite, on a shared linear axis.
func Chart(title, subtitle string, pairs []Pair) string {
	const (
		left, right, top = 230.0, 120.0, 86.0
		width            = 960.0
		bar, gap, group  = 16.0, 4.0, 22.0
	)
	plotW := width - left - right
	rowH := 2*bar + gap + group
	height := top + rowH*float64(len(pairs)) + 64
	var maxD time.Duration
	for _, p := range pairs {
		maxD = max(maxD, p.Unsafe, p.Safe)
	}
	step := niceStep(maxD.Seconds(), 6)
	axisMax := math.Ceil(maxD.Seconds()/step) * step
	if axisMax <= 0 {
		axisMax = 1
	}
	x := func(sec float64) float64 { return left + sec/axisMax*plotW }

	var b strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	f(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="%s">`+"\n", width, height, width, height, fontF)
	f(`<rect width="100%%" height="100%%" fill="#ffffff"/>` + "\n")
	f(`<text x="24" y="30" font-size="17" font-weight="600" fill="%s">%s</text>`+"\n", inkC, esc(title))
	f(`<text x="24" y="50" font-size="12.5" fill="%s">%s</text>`+"\n", mutedC, esc(subtitle))
	plotH := rowH * float64(len(pairs))
	for v := 0.0; v <= axisMax+1e-9; v += step {
		f(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`+"\n", x(v), top-8, x(v), top+plotH-group+8, gridC)
		f(`<text x="%.1f" y="%.1f" font-size="11" text-anchor="middle" fill="%s">%s s</text>`+"\n", x(v), top+plotH-group+24, mutedC, trimFloat(v))
	}
	for i, p := range pairs {
		y := top + float64(i)*rowH
		f(`<text x="%.1f" y="%.1f" font-size="13" font-weight="600" text-anchor="end" fill="%s">%s</text>`+"\n", left-12, y+bar+gap/2+4, inkC, esc(p.Name))
		for k, v := range []struct {
			d    time.Duration
			c    string
			what string
		}{{p.Unsafe, unsafeC, p.UnsafeWhat}, {p.Safe, safeC, p.SafeWhat}} {
			by := y + float64(k)*(bar+gap)
			w := math.Max(x(v.d.Seconds())-left, 1.5)
			f(`<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n", left, by, w, bar, v.c)
			label := Dur(v.d)
			if v.d == 0 {
				label = "none"
			}
			if v.what != "" {
				label += " · " + v.what
			}
			f(`<text x="%.1f" y="%.1f" font-size="11.5" fill="%s">%s</text>`+"\n", left+w+6, by+bar-4, inkC, esc(label))
		}
	}
	ly := top + plotH - group + 48
	f(`<rect x="%.1f" y="%.1f" width="14" height="12" fill="%s"/>`+"\n", left, ly-10, unsafeC)
	f(`<text x="%.1f" y="%.1f" font-size="12" fill="%s">as usually written</text>`+"\n", left+20, ly, mutedC)
	f(`<rect x="%.1f" y="%.1f" width="14" height="12" fill="%s"/>`+"\n", left+160, ly-10, safeC)
	f(`<text x="%.1f" y="%.1f" font-size="12" fill="%s">safe rewrite</text>`+"\n", left+180, ly, mutedC)
	b.WriteString("</svg>\n")
	return b.String()
}

func niceStep(span, target float64) float64 {
	if span <= 0 {
		return 1
	}
	raw := span / target
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= raw {
			return m * p
		}
	}
	return 10 * p
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
