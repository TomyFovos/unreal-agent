package analysis

import (
	"fmt"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func timestamp(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// TelemetryLines is shared by /analyze and conversation Markdown exports.
func TelemetryLines(r Report, kinds ...string) []string {
	include := func(kind string) bool {
		if len(kinds) == 0 {
			return true
		}
		for _, k := range kinds {
			if k == kind {
				return true
			}
		}
		return false
	}
	var lines []string
	for _, a := range r.Activities {
		if !include(a.Kind) {
			continue
		}
		line := a.Kind + " " + viewer.SafeText(a.ID) + " start=" + timestamp(a.StartedAt)
		switch a.Kind {
		case "claude_usage":
			lines = append(lines, "Claude Code turn usage "+viewer.SafeText(a.ID)+" at="+timestamp(a.EndedAt)+" (turn total; already included in response totals)")
			if a.Usage == nil {
				lines = append(lines, "usage unknown")
			} else {
				u := viewer.Usage{}
				viewer.AddUsage(&u, *a.Usage)
				lines = append(lines, usageLines(u)...)
			}
			continue
		case "host_start":
			line += " revision=" + viewer.SafeText(a.Revision)
			if a.Modified {
				line += " modified=true"
			}
		case "child_start":
			line += " template=" + viewer.SafeText(a.Template) + " model=" + viewer.SafeText(a.Model) + " effort=" + viewer.SafeText(a.Effort)
		case "native_bash":
			elapsed := "unknown"
			if !a.StartedAt.IsZero() && !a.EndedAt.IsZero() && !a.EndedAt.Before(a.StartedAt) {
				elapsed = a.EndedAt.Sub(a.StartedAt).String()
			}
			line += " end=" + timestamp(a.EndedAt) + " elapsed=" + elapsed
		}
		if a.Reason != "" {
			line += " reason=" + viewer.SafeText(a.Reason)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	if include("claude_compact") {
		for _, c := range r.Compactions {
			status := "completed"
			if c.Interrupted {
				status = "interrupted"
			} else if c.Error != "" {
				status = "failed"
			}
			elapsed := "unknown"
			if !c.StartedAt.IsZero() && !c.EndedAt.IsZero() && !c.EndedAt.Before(c.StartedAt) {
				elapsed = c.EndedAt.Sub(c.StartedAt).String()
			}
			line := fmt.Sprintf("Claude compact %s start=%s end=%s elapsed=%s before=%d", status, timestamp(c.StartedAt), timestamp(c.EndedAt), elapsed, c.BeforeTokens)
			if c.InterruptReason != "" {
				line += " reason=" + viewer.SafeText(c.InterruptReason)
			}
			if c.Error != "" {
				line += " error=" + viewer.SafeText(c.Error)
			}
			lines = append(lines, line)
			if c.Usage == nil {
				lines = append(lines, "compact usage unknown (reasoning unknown)")
			} else {
				u := viewer.Usage{}
				viewer.AddUsage(&u, *c.Usage)
				lines = append(lines, "compact usage:")
				lines = append(lines, usageLines(u)...)
			}
		}
	}
	return lines
}
