package analysis

import (
	"fmt"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const SubscriptionUsageView = "Subscription usage"

type SubscriptionWindow struct {
	llm.SubscriptionObservation
	State string // observed, unknown, stale, or reset_since
}

// SubscriptionWindows includes the known provider windows even when the
// provider has never reported them. Additional recorded windows also survive.
func SubscriptionWindows(observations llm.SubscriptionObservations, now time.Time) []SubscriptionWindow {
	latest := llm.SubscriptionObservations{
		{Provider: "openai-codex", SubscriptionUsage: llm.SubscriptionUsage{Window: "primary"}},
		{Provider: "openai-codex", SubscriptionUsage: llm.SubscriptionUsage{Window: "secondary"}},
		{Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "five_hour"}},
		{Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "seven_day"}},
		{Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "seven_day_opus"}},
		{Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "seven_day_sonnet"}},
		{Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "overage"}},
	}
	latest.Merge(observations)
	windows := make([]SubscriptionWindow, 0, len(latest))
	for _, o := range latest.Clone() {
		if o.Window == "" {
			continue
		}
		state := "observed"
		if o.ResetPassed(now) {
			state, o.UsedPercent = "reset_since", nil
		} else if o.UsedPercent == nil && o.HasWindow() && !o.PercentInvalidatedAt.IsZero() {
			state = "stale"
			if o.Status == "rejected" && o.PercentInvalidatedAt.Equal(o.ObservedAt) {
				o.Status = ""
			}
		} else if o.UsedPercent == nil || !o.HasWindow() {
			state, o.UsedPercent = "unknown", nil
		}
		windows = append(windows, SubscriptionWindow{SubscriptionObservation: o, State: state})
	}
	return windows
}

func SubscriptionProviderName(provider string) string {
	switch provider {
	case "openai-codex":
		return "ChatGPT"
	case "claude":
		return "Claude"
	case "opencode-go":
		return "OpenCode Go"
	default:
		return provider
	}
}

func subscriptionLines(r Report) []string {
	var out []string
	if r.Tree != nil && r.Tree.Partial {
		out = append(out, "partial Session tree")
	}
	if len(r.SubscriptionUsage) == 0 {
		return append(out, "subscription usage unknown")
	}
	for _, w := range r.SubscriptionUsage {
		fields := []string{SubscriptionProviderName(w.Provider) + " " + w.Window}
		if w.UsedPercent != nil {
			fields = append(fields, percentText(min(100, max(0, 100-*w.UsedPercent)))+"% left", "used "+percentText(*w.UsedPercent)+"%")
		} else if w.State == "stale" {
			fields = append(fields, "—")
		} else if w.State != "reset_since" {
			fields = append(fields, "unknown")
		}
		if w.Status != "" {
			fields = append(fields, w.Status)
		}
		if !w.ResetsAt.IsZero() {
			label := "resets "
			if w.State == "reset_since" {
				label = "reset since "
			}
			fields = append(fields, label+w.ResetsAt.Format(time.RFC3339))
		}
		if !w.ObservedAt.IsZero() {
			fields = append(fields, "observed "+w.ObservedAt.Format(time.RFC3339))
		}
		out = append(out, strings.Join(fields, " · "))
	}
	return out
}

func percentText(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}
