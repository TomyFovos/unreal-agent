package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	ledgerusage "github.com/unreallabsai/unreal-agent/harness/usage"
)

func TestReportedChargesAndBudgetOverrunInAnalysis(t *testing.T) {
	now := time.Now()
	var b ledgerusage.WindowBuilder
	for _, c := range []llm.Charge{
		{Currency: "USD", BillingSource: "go", Charged: new(0.6)},
		{Currency: "USD", BillingSource: "go-plus", Charged: new(1.2)},
	} {
		b.Add("worker", ledgerusage.Sample{At: now, Provider: "opencode-go", Model: "deepseek", Calls: 1, Charge: &c})
	}
	r := analysis.Report{At: now, Dashboard: &analysis.Dashboard{Children: []analysis.ChildSignal{{ID: "w", Title: "Monitor", State: "running"}}},
		Ledger: &subagent.UsageReport{All: b.Window(), Spend: []ledgerusage.Spend{{Provider: "opencode-go", Currency: "USD", Hourly: new(6.1)}},
			Sessions: []subagent.SessionUsage{{ID: "w", BudgetSteps: 1, Subtree: ledgerusage.Totals{Calls: 2}}}}}
	for _, tc := range []struct {
		view string
		want []string
	}{
		{"Usage", []string{"deepseek / worker · go · $0.600 · $0.600000/call", "go-plus · $1.200 · $1.200000/call", "EMA $6.10/h"}},
		{"Agents", []string{"Calls / budget", "! 2/1"}},
	} {
		var text strings.Builder
		for _, row := range analysisRows(r, analysisTarget{view: tc.view, width: 120}, Theme{}) {
			text.WriteString(row.plain() + "\n")
		}
		for _, want := range tc.want {
			if !strings.Contains(text.String(), want) {
				t.Fatalf("%s omitted %q:\n%s", tc.view, want, text.String())
			}
		}
	}
}

func TestFooterShowsSpendAfterAllowanceWhenSpacePermits(t *testing.T) {
	now := time.Now()
	s := Snapshot{SubscriptionUsage: llm.SubscriptionObservations{{Provider: "openai-codex", ObservedAt: now,
		SubscriptionUsage: llm.SubscriptionUsage{Window: "primary", ResetsAt: now.Add(time.Hour), UsedPercent: new(13.0), WindowMinutes: new(int64(300))}}}}
	u := UIState{Now: now, Spend: []ledgerusage.Spend{{Provider: "opencode-go", Currency: "USD", Today: new(87.8), Hourly: new(6.1)}}}
	wide := statisticsBar(s, u, makeLayout(190, 24)).plain()
	if !strings.Contains(wide, "ChatGPT") || !strings.Contains(wide, "OpenCode Go 今日 $87.8 · $6.1/h") {
		t.Fatal(wide)
	}
	narrow := statisticsBar(s, u, makeLayout(75, 24)).plain()
	if !strings.Contains(narrow, "ChatGPT") || strings.Contains(narrow, "OpenCode Go") {
		t.Fatal(narrow)
	}
}
