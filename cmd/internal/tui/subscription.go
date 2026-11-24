package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func subscriptionWindows(s Snapshot, u UIState) []analysis.SubscriptionWindow {
	if len(s.SubscriptionUsage)+len(u.Viewer.SubscriptionUsage) == 0 {
		return nil
	}
	usage := slices.Clone(s.SubscriptionUsage)
	usage.Merge(u.Viewer.SubscriptionUsage)
	return analysis.SubscriptionWindows(usage, u.Now)
}

// subscriptionBar fits complete windows and shows a provider name once per
// group, including when the first window of that provider does not fit.
func subscriptionBar(windows []analysis.SubscriptionWindow, theme Theme, width, resetSpace int) line {
	var out line
	var provider string
	for _, w := range windows {
		if w.UsedPercent == nil && w.State != "stale" {
			continue
		}
		label := subscriptionWindowLabel(w, theme)
		text := label + " " + theme.symbol("—", "-")
		if w.UsedPercent != nil {
			text = fmt.Sprintf("%s %s%.0f%%", label, theme.symbol("残り", "left "), remainingPercent(*w.UsedPercent))
		}
		if w.Provider != provider {
			text = analysis.SubscriptionProviderName(w.Provider) + " " + text
		}
		part := textLine(theme.symbol(" · ", " / ")+strings.Join(strings.Fields(text), " "), meta)
		if resetSpace > 0 && w.UsedPercent != nil && !w.ResetsAt.IsZero() {
			format := "01/02 15:04"
			if label == "5h" {
				format = "15:04"
			}
			local := w.ResetsAt.Local()
			reset := textLine(" ("+local.Format(format)+") "+displayZone(local), meta)
			if reset.width() <= resetSpace {
				part = append(part, reset...)
				resetSpace -= reset.width()
			}
		}
		if part.width() <= width {
			out = append(out, part...)
			width -= part.width()
			provider = w.Provider
		}
	}
	return out
}

func subscriptionWindowLabel(w analysis.SubscriptionWindow, theme Theme) string {
	week := theme.symbol("週", "7d")
	switch w.Window {
	case "five_hour":
		return "5h"
	case "seven_day":
		return week
	case "seven_day_opus":
		return "Opus" + week
	case "seven_day_sonnet":
		return "Sonnet" + week
	case "overage":
		return theme.symbol("超過", "extra")
	}
	if w.WindowMinutes != nil {
		switch *w.WindowMinutes {
		case 300:
			return "5h"
		case 10080:
			return week
		default:
			return fmt.Sprintf("%dm", *w.WindowMinutes)
		}
	}
	switch w.Window {
	case "primary":
		return "P"
	case "secondary":
		return "S"
	default:
		return w.Window
	}
}

func nextSubscriptionReset(s Snapshot, u UIState) time.Time {
	var next time.Time
	for _, observations := range []llm.SubscriptionObservations{s.SubscriptionUsage, u.Viewer.SubscriptionUsage} {
		for _, o := range observations {
			if (o.UsedPercent != nil || !o.PercentInvalidatedAt.IsZero()) && o.ResetsAt.After(u.Now) && (next.IsZero() || o.ResetsAt.Before(next)) {
				next = o.ResetsAt
			}
		}
	}
	return next
}

// remainingPercent shows what is left of a window, clamped to 0–100.
func remainingPercent(used float64) float64 {
	return min(100, max(0, 100-used))
}
