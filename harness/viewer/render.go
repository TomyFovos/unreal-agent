package viewer

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"strings"
	"time"
	"unicode"
)

// SafeText removes terminal controls (including escape sequences' ESC). Callers
// should never render raw provider text, paths, IDs, or failure strings directly.
func SafeText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			return ' '
		}
		return r
	}, value)
}
func FormatUsage(u Usage) string {
	if !u.Known {
		return "unknown"
	}
	suffix := ""
	if u.Partial {
		suffix = " (partial)"
	}
	return fmt.Sprintf("in %d / out %d%s", u.Input, u.Output, suffix)
}
func FormatDuration(d Duration) string {
	if !d.Known {
		return "unknown"
	}
	return d.Value.Round(time.Second).String()
}

// RenderRows can be embedded in a TUI child panel without owning its event loop.
func RenderRows(rows []Row, selected session.ID) string {
	var b strings.Builder
	for _, r := range rows {
		marker := " "
		if r.ID == selected {
			marker = ">"
		}
		fmt.Fprintf(&b, "%s %s%s", marker, strings.Repeat("  ", min(r.Depth, 32)), SafeText(string(r.ID)))
		if r.Label != "" {
			fmt.Fprintf(&b, " (%s)", SafeText(r.Label))
		}
		if r.ParentID != "" {
			fmt.Fprintf(&b, " | parent op: %s", SafeText(string(r.ParentOperationStatus)))
		}
		fmt.Fprintf(&b, " | runtime: %s | elapsed: %s | usage: %s", r.Runtime, FormatDuration(r.Elapsed), FormatUsage(r.Usage))
		if r.Finish != nil {
			fmt.Fprintf(&b, " | Finish: %s", SafeText(r.Finish.Status))
		}
		if r.NeedsResync {
			b.WriteString(" | resync required")
		}
		if r.Problem != "" {
			fmt.Fprintf(&b, " | %s", SafeText(r.Problem))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func RenderDetail(d Detail) string {
	var b strings.Builder
	r := d.Row
	fmt.Fprintf(&b, "%s\n", SafeText(string(r.ID)))
	if r.ParentID != "" {
		fmt.Fprintf(&b, "Parent: %s / operation: %s (%s)\n", SafeText(string(r.ParentID)), SafeText(string(r.ParentOperationID)), SafeText(string(r.ParentOperationStatus)))
	}
	fmt.Fprintf(&b, "Runtime observation: %s\nElapsed wall time: %s\nTokens: %s\n", r.Runtime, FormatDuration(r.Elapsed), FormatUsage(r.Usage))
	fmt.Fprintf(&b, "History cursor: %d (more: %t)\n", r.Cursor, r.More)
	if r.Activity != "" {
		fmt.Fprintf(&b, "Recent activity: %s\n", SafeText(r.Activity))
	}
	if r.Failure != "" {
		fmt.Fprintf(&b, "Runtime failure: %s\n", SafeText(r.Failure))
	}
	for _, op := range r.Operations {
		fmt.Fprintf(&b, "Operation %s [%s]: %s; elapsed %s\n", SafeText(string(op.ID)), SafeText(op.Tool), SafeText(string(op.Status)), FormatDuration(op.Elapsed))
	}
	if f := r.Finish; f != nil {
		fmt.Fprintf(&b, "Canonical Finish: %s\n%s\n", SafeText(f.Status), SafeText(f.Summary))
		for _, file := range f.ChangedFiles {
			fmt.Fprintf(&b, "Changed: %s\n", SafeText(file))
		}
		for _, test := range f.Tests {
			fmt.Fprintf(&b, "Test: %s\n", SafeText(test))
		}
		for _, blocker := range f.Blockers {
			fmt.Fprintf(&b, "Blocker: %s\n", SafeText(blocker))
		}
	} else {
		b.WriteString("Canonical Finish: unknown\n")
	}
	if r.Problem != "" {
		fmt.Fprintf(&b, "Projection: %s\n", SafeText(r.Problem))
	}
	return b.String()
}
