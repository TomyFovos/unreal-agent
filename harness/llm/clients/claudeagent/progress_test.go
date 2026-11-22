package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestAgentProgressAndNestedTranscript(t *testing.T) {
	for _, tc := range []struct{ name, in, chosen string }{
		{"the call's own model", "haiku", "sonnet"},
		{"the model chosen with subagent model", "", "opus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "agent_progress", "CLAUDEAGENT_AGENT_MODEL="+tc.in)
			h.client.config.Subagents = SubagentCLI{AgentModel: func() string { return tc.chosen }}
			ctx, _, _ := withMode(t, permission.FullAccess)
			var receipts []llm.ToolOutcome
			response, err := h.client.Respond(ctx, llm.Request{Input: []llm.Item{user("Review the parser")}}, llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "session-1"},
				Tools: func(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
					outcomes, err := h.tools(ctx, response)
					receipts = append(receipts, outcomes...)
					return outcomes, err
				},
				Progress: func(p llm.Progress) { h.progress.WriteString(p.Delta) },
			})
			if err != nil {
				t.Fatal(err)
			}
			model := tc.in
			if model == "" {
				model = tc.chosen
			}
			wantProgress := "\n› Claude Code Agent · " + model + ": Review the parser · running\n"
			if got := h.progress.String(); got != wantProgress {
				t.Fatalf("progress = %q, want %q", got, wantProgress)
			}
			if len(h.responses) != 1 {
				t.Fatalf("rounds = %d, want 1", len(h.responses))
			}
			recorded := h.responses[0].Output
			if len(recorded) != 1 {
				t.Fatalf("round = %+v, want just the Agent call", recorded)
			}
			if call := recorded[0].Data.(llm.ToolCall); recorded[0].Type != llm.ItemToolCall || call.CallID != "toolu_agent" || call.Name != tool.ClaudeCodeName {
				t.Fatalf("recorded call = %+v", call)
			}
			want := []llm.ToolObservation{
				{ParentCallID: "toolu_agent", Type: llm.ItemReasoning, Model: "claude-sonnet-5-5", Text: "Start with the parser"},
				{ParentCallID: "toolu_agent", Type: llm.ItemMessage, Model: "claude-sonnet-5-5", Text: "I will read the parser now."},
				{ParentCallID: "toolu_agent", Type: llm.ItemToolCall, Model: "claude-sonnet-5-5", CallID: "toolu_read", Tool: "Read"},
				{ParentCallID: "toolu_agent", Type: llm.ItemToolResult, CallID: "toolu_read", Text: "Parser source", IsError: true},
			}
			encoded, err := json.Marshal(recorded[0])
			if err != nil {
				t.Fatal(err)
			}
			var persisted llm.Item
			if err := json.Unmarshal(encoded, &persisted); err != nil || !slices.Equal(persisted.ToolObservations, want) || strings.Contains(string(encoded), "private-signature") {
				t.Fatalf("structured history = %s, observations = %+v, err = %v", encoded, persisted.ToolObservations, err)
			}
			if len(receipts) != 1 || receipts[0].Result.CallID != "toolu_agent" || receipts[0].Failed || !slices.Equal(receipts[0].Result.Output,
				[]llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "The parser reads one file twice."}}) {
				t.Fatalf("parent receipt = %+v", receipts)
			}
			if len(response.Output) != 1 || response.Output[0].Data.(llm.Message).Text != "Done." {
				t.Fatalf("final response = %+v", response.Output)
			}
			// The parent's final answer stays separate from native observations.
			if records(response.Output, "toolu_read") || records(response.Output, "I will read the parser now.") || records(response.Output, "/tmp/parser.go") {
				t.Fatalf("native observations replaced the final answer: %+v", response.Output)
			}
		})
	}
}

// records reports whether a recorded item carries the text or call id.
func records(items []llm.Item, needle string) bool {
	for _, item := range items {
		switch data := item.Data.(type) {
		case llm.Message:
			if strings.Contains(data.Text, needle) {
				return true
			}
		case llm.ToolCall:
			if data.CallID == needle || strings.Contains(data.Arguments, needle) {
				return true
			}
		}
	}
	return false
}

// The Agent call's subject names the model and the task, never the JSON input.
func TestAgentCallSubjectNamesTheModelAndTheTask(t *testing.T) {
	chosen := func() string { return "sonnet" }
	for _, tc := range []struct {
		name, input string
		chosen      func() string
		want        string
	}{
		{"the call's own model wins", `{"model":"haiku","description":"Review the parser"}`, chosen, "Claude Code Agent · haiku: Review the parser"},
		{"the chosen model when the call names none", `{"description":"Review the parser"}`, chosen, "Claude Code Agent · sonnet: Review the parser"},
		{"no model leaves no model part", `{"description":"Review the parser"}`, func() string { return "" }, "Claude Code Agent: Review the parser"},
		{"no choice at all", `{"description":"Review the parser"}`, nil, "Claude Code Agent: Review the parser"},
		{"no description", `{"model":"haiku","prompt":"the whole task"}`, nil, "Claude Code Agent · haiku"},
		{"nothing known", `{"prompt":"the whole task"}`, nil, "Claude Code Agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCallSubject(jsontext.Value(tc.input), tc.chosen); got != tc.want {
				t.Fatalf("subject = %q, want %q", got, tc.want)
			}
		})
	}
	// The subject is one clipped line, like every other progress subject.
	long := `{"model":"haiku","description":"` + strings.Repeat("long task ", 100) + `"}`
	got := agentCallSubject(jsontext.Value(long), nil)
	if len(got) > 512 || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "Claude Code Agent · haiku: long task") || strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("clipped subject = %q (%d bytes)", got, len(got))
	}
}

// The chosen model is read only when there is a choice.
func TestAgentModelIsNilSafe(t *testing.T) {
	if model := (SubagentCLI{}).agentModel(); model != "" {
		t.Fatalf("model = %q without a choice", model)
	}
	if model := (SubagentCLI{AgentModel: func() string { return "opus" }}).agentModel(); model != "opus" {
		t.Fatalf("model = %q, want the choice", model)
	}
}
