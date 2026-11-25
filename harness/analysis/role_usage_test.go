package analysis

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestUsageCountsModelCallsByRoleAndPairsMeasuredInputs(t *testing.T) {
	selectRuntime := func(f *measurementFixture, provider, model string) {
		f.add(0, sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{Provider: provider, Model: model}})
	}
	native := func(id string, inputs ...llm.Usage) *Accumulator {
		f := &measurementFixture{Accumulator: New(session.ID(id))}
		selectRuntime(f, "opencode-go", "deepseek")
		for i, usage := range inputs {
			turn := string(rune('a' + i))
			f.turn(i*2, turn, contextengine.WindowThreeLayer)
			f.respond(i*2+1, turn, "opencode-go", "deepseek", "reply", usage)
		}
		return f.Accumulator
	}
	lead := native("lead", llm.Usage{InputTokens: 165_000, CachedInputTokens: 162_000}, llm.Usage{InputTokens: 167_000},
		llm.Usage{InputTokens: 999_999, Unknown: []llm.UsageField{llm.UsageInput}})
	worker := native("worker-a", llm.Usage{InputTokens: 1000}, llm.Usage{InputTokens: 3000})
	cli := &measurementFixture{Accumulator: New("worker-b")}
	selectRuntime(cli, "claude", "opus")
	cli.turn(0, "exec", contextengine.WindowThreeLayer)
	stream := func(id string, n int64) {
		cli.activity(1, llm.ActivityRecord{Kind: "claude_stream", Provider: "claude", Model: "opus", ID: id,
			Reason: `{"event":"message_start"}`, RequestUsage: &llm.RequestUsage{Input: &n}})
	}
	stream("exec/stream/1", 6000)
	stream("exec/stream/1", 6000) // a duplicate activity is still one call
	cli.add(2, sessionstore.ModelResponse{TurnID: "exec", Response: llm.Response{Provider: "claude", Model: "opus",
		Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: tool.ClaudeCodeName, CallID: "batch"}}}}})
	cli.add(3, session.Turn{ID: "continued", Type: session.TurnRegular, ToolContinuation: "exec"})
	stream("exec/stream/2", 14_000)
	cli.activity(5, llm.ActivityRecord{Kind: "claude_usage", Provider: "claude", ID: "exec", Model: "opus",
		Usage: &llm.Usage{InputTokens: 20_000}})
	cli.respond(6, "continued", "claude", "opus", "final", llm.Usage{InputTokens: 20_000})

	legacy := &measurementFixture{Accumulator: New("collector")}
	selectRuntime(legacy, "claude", "opus")
	legacy.turn(0, "old", contextengine.WindowThreeLayer)
	legacy.respond(1, "old", "claude", "opus", "legacy invocation total", llm.Usage{InputTokens: 24_000})

	tree := &Tree{nodes: []*treeNode{
		{acc: New("main"), observed: true},
		{acc: lead, layer: "lead", parent: "main", observed: true},
		{acc: worker, layer: "worker", parent: "lead", observed: true},
		{acc: cli.Accumulator, layer: "worker", parent: "lead", observed: true},
		{acc: legacy.Accumulator, layer: "collector", parent: "lead", observed: true},
	}}
	report := tree.Report(measurementEpoch, nil)
	rows := make(map[string]RoleUsage)
	for _, row := range report.RoleUsage {
		rows[row.Role] = row
	}
	l, w, c := rows["lead"], rows["worker"], rows["collector"]
	if len(rows) != 4 || !l.Calls.Known || l.Calls.Value != 3 || l.Calls.Unknown != 0 ||
		!l.InputTokensPerCall.Known || l.InputTokensPerCall.Value != 166_000 || l.InputTokensPerCall.Unknown != 1 ||
		!w.Calls.Known || w.Calls.Value != 4 || w.Calls.Unknown != 0 || !w.InputTokensPerCall.Known || w.InputTokensPerCall.Value != 6000 ||
		c.Calls.Known || c.Calls.Unknown != 1 || c.InputTokensPerCall.Known {
		t.Fatalf("incorrect observed call usage: %+v", rows)
	}
	if view := strings.Join(Lines(report, "Usage", false), "\n"); !strings.Contains(view, "worker  4 (measured) calls  6000 (derived) tokens/call") {
		t.Fatal("Usage export omitted the model calls per role")
	}
}
