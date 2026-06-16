package preflight

import (
	"fmt"
	"io"
	"strings"
)

// RenderTable writes a human-readable PASS/WARN/FAIL table of the report to w,
// followed by a one-line verdict. It is the CLI surface (`tms trade preflight`).
func RenderTable(w io.Writer, r Report) {
	fmt.Fprintf(w, "PREFLIGHT  mode=%s  strategy=%s  (%s)\n", r.Mode, r.Strategy, r.TS.Format("2006-01-02 15:04:05Z07:00"))
	fmt.Fprintln(w, strings.Repeat("-", 78))

	// Column widths.
	const (
		statusW = 5
		checkW  = 26
		sevW    = 8
	)
	fmt.Fprintf(w, "%-*s  %-*s  %-*s  %s\n", statusW, "STAT", checkW, "CHECK", sevW, "SEVERITY", "DETAIL")
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-*s  %-*s  %-*s  %s\n",
			statusW, mark(c.Status), checkW, c.Check, sevW, c.Severity, c.Detail)
	}
	fmt.Fprintln(w, strings.Repeat("-", 78))

	if r.OK {
		warns := r.Warnings()
		if len(warns) > 0 {
			fmt.Fprintf(w, "RESULT: PASS (with %d warning(s)) — go-live preconditions met\n", len(warns))
		} else {
			fmt.Fprintln(w, "RESULT: PASS — all go-live preconditions met")
		}
		return
	}
	blockers := r.Blockers()
	fmt.Fprintf(w, "RESULT: FAIL — %d blocker(s) must be resolved before go-live:\n", len(blockers))
	for _, b := range blockers {
		fmt.Fprintf(w, "  - %s: %s\n", b.Check, b.Detail)
	}
}

// mark renders a status as a fixed-width glyph for the table.
func mark(s Status) string {
	switch s {
	case StatusPass:
		return "PASS"
	case StatusWarn:
		return "WARN"
	case StatusFail:
		return "FAIL"
	case StatusSkip:
		return "SKIP"
	default:
		return string(s)
	}
}
