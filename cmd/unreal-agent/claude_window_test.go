//go:build linux || darwin

package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func claudeCallOption(call testclaude.Call, name string) string {
	i := slices.Index(call.Arguments, name)
	if i >= 0 && i+1 < len(call.Arguments) {
		return call.Arguments[i+1]
	}
	return ""
}

func TestClaudeThreeLayerWindowResumesAfterHostRestart(t *testing.T) {
	t.Setenv("DISABLE_AUTO_COMPACT", "0")
	f := testclaude.New(t)
	cfg := claudeRouteConfiguration(t, f)
	sessions := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "fixture.txt"), []byte("tool receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	const id = "claude-window"
	const verbatim = "  Continue from the recorded state\r\n\t続き\n"
	const question = "【要確認】 Which target should I use?"
	var currentID string
	if !t.Run("build-window", func(t *testing.T) {
		f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentAutoCompact: true, AgentFinal: question, AgentUsage: []jsontext.Value{
			jsontext.Value(`{"input_tokens":199950,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}`),
		}})
		client, _, _ := startInteractiveHost(t, cfg, "--normal-runtime", "--session-directory", sessions)
		view, err := client.Open(t.Context(), host.Create, id)
		if err != nil {
			t.Fatal(err)
		}
		submitInteractive(t, client, view, "initial", "Old request available through its record.")
		first := inspectInteractive(t, client, id, func(v host.View) bool { return hasFinalText(v, question) && v.Progress == nil })
		if calls := f.Calls(t); len(calls) != 1 {
			t.Fatalf("crossing W in the final answer started %d CLI turns, want one", len(calls))
		}
		for _, item := range first.History.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil && r.Context.Kind == sessionstore.ContextWindowBoundary {
				t.Fatal("crossing W in the final answer must wait for the next request")
			}
		}

		f.Set(t, testclaude.Config{
			Catalog: hostClaudeCatalog, AgentAutoCompact: true, AgentFinal: "rebuilt window answered",
			AgentSteps: []testclaude.AgentStep{{Tool: "Read", Input: jsontext.Value(`{"file_path":"fixture.txt"}`)}},
			AgentUsage: []jsontext.Value{
				jsontext.Value(`{"input_tokens":300,"cache_read_input_tokens":0,"cache_creation_input_tokens":77}`),
				jsontext.Value(`{"input_tokens":700,"cache_read_input_tokens":88,"cache_creation_input_tokens":99}`),
			},
		})
		submitInteractive(t, client, view, "boundary", verbatim)
		end := inspectInteractive(t, client, id, func(v host.View) bool {
			return hasFinalText(v, "rebuilt window answered") && v.Progress == nil
		})
		calls := f.Calls(t)
		if len(calls) != 2 {
			t.Fatalf("want two invocations, got %d", len(calls))
		}
		oldID := claudeCallOption(calls[0], "--session-id")
		currentID = claudeCallOption(calls[1], "--session-id")
		if oldID == "" || currentID == "" || currentID == oldID || claudeCallOption(calls[1], "--resume") != "" {
			t.Fatalf("window did not start a fresh conversation: %q -> %q", oldID, currentID)
		}
		prompt := calls[1].Input
		if !strings.HasPrefix(prompt, "Unreal current state at the window boundary (recorded data): Earlier conversation is in L3.") ||
			strings.Contains(prompt, question) || !strings.HasSuffix(prompt, verbatim) ||
			strings.Contains(prompt, "Old request available") || strings.Contains(prompt, "fixture private") {
			t.Fatalf("unexpected first prompt of the window: %q", prompt)
		}
		for _, call := range calls {
			if !slices.Contains(call.Environment, "DISABLE_AUTO_COMPACT=1") ||
				!strings.Contains(claudeCallOption(call, "--settings"), `"DISABLE_AUTO_COMPACT":"1"`) ||
				claudeCallOption(call, "--autocompact") != "" ||
				!strings.Contains(claudeCallOption(call, "--append-system-prompt"), "You are running inside Unreal Agent.") {
				t.Fatal("three-layer launch did not set its process-local compaction and system options")
			}
		}
		var boundary record.Locator
		var after, headerTokens int64
		var bound, admitted, cached bool
		for _, item := range end.History.Items {
			switch data := item.Data.(type) {
			case sessionstore.HostRecord:
				r := data.Context
				if r == nil {
					continue
				}
				if r.Conversation != nil && r.Conversation.Compacted {
					t.Fatal("Claude Code compacted before the Unreal boundary")
				}
				if r.Kind == sessionstore.ContextWindowBoundary {
					if boundary != "" || !slices.Contains(r.Window.Causes, "threshold") || r.Window.ThresholdTokens != 200_000 ||
						r.Window.BeforeMeasurement != "measured-prefix+estimated-delta" || r.Window.BeforeTokens < 200_000 ||
						r.Window.HeaderTokens == nil || *r.Window.HeaderTokens <= 0 {
						t.Fatalf("unexpected boundary: %+v", r.Window)
					}
					boundary = record.Item(id, uint64(item.Sequence))
					after = r.Window.AfterTokens
					headerTokens = *r.Window.HeaderTokens
					_, raw, _ := strings.Cut(r.Text, "\n")
					var state contextstate.State
					if err := json.Unmarshal([]byte(raw), &state); err != nil || len(state.QuestionsToUser.Current) != 0 ||
						state.QuestionsToUser.Processed.Count != 1 || state.QuestionsToUser.Processed.Locator != "rec:"+id+"/context/questions_to_user" {
						t.Fatalf("prior question did not settle with L3 evidence: %+v %v", state.QuestionsToUser, err)
					}
				}
				if r.Kind == sessionstore.ContextConversationBinding && r.ConversationBinding.ID == currentID {
					bound = boundary != "" && r.ConversationBinding.Boundary == string(boundary)
				}
			case session.Turn:
				if a := data.ContextAdmission; a != nil && a.WindowBoundary != "" {
					want := headerTokens + (contextengine.TokenEstimator{}).Text(prompt)
					admitted = a.WindowBoundary == boundary && a.InputTokens == want && after == want && a.Measurement == "estimated"
				}
			case sessionstore.ModelResponse:
				if cache := data.WindowCache; cache.Boundary != "" {
					if cached || cache.Boundary != boundary || !cache.ReadKnown || cache.Read != 0 || !cache.WriteKnown || cache.Write != 77 {
						t.Fatalf("incorrect first model call cache usage: %+v", cache)
					}
					cached = true
				}
			}
		}
		if !bound || !admitted || !cached {
			t.Fatalf("missing window evidence: binding=%v admission=%v cache=%v", bound, admitted, cached)
		}
		stopMultiSession(t, client, view)
	}) {
		return
	}

	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "resumed the rebuilt window"})
	client, _, _ := startInteractiveHost(t, cfg, "--normal-runtime", "--session-directory", sessions)
	view, err := client.Open(t.Context(), host.Resume, id)
	if err != nil {
		t.Fatal(err)
	}
	const followup = "  Continue after the Host restart.\n"
	submitInteractive(t, client, view, "resumed", followup)
	inspectInteractive(t, client, id, func(v host.View) bool { return hasFinalText(v, "resumed the rebuilt window") })
	calls := f.Calls(t)
	if len(calls) != 3 || claudeCallOption(calls[2], "--resume") != currentID || calls[2].Input != followup {
		t.Fatal("restart did not resume the mapped conversation with just the verbatim follow-up")
	}
	stopMultiSession(t, client, view)
}

func TestClaudeSummaryWindowKeepsCLICompaction(t *testing.T) {
	t.Setenv("DISABLE_AUTO_COMPACT", "0")
	f := testclaude.New(t)
	cfg := claudeRouteConfiguration(t, f)
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "fixture.txt"), []byte("tool receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	f.Set(t, testclaude.Config{
		Catalog: hostClaudeCatalog, AgentAutoCompact: true, AgentInputTokens: 205_000, AgentFinal: "summary strategy answered",
		AgentSteps:           []testclaude.AgentStep{{Tool: "Read", Input: jsontext.Value(`{"file_path":"fixture.txt"}`)}},
		AgentStepInputTokens: 205_000,
	})
	cfg.Context.WindowStrategy = contextengine.WindowSummary
	cfg.Context.WindowThresholdTokens = 100
	cfg.Claude.CompactThresholdTokens = 100_000
	c, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
	const id = "claude-summary-strategy"
	view, err := c.Open(t.Context(), host.Create, id)
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, view, "first", "First summary turn.")
	inspectInteractive(t, c, id, func(v host.View) bool { return hasFinalText(v, "summary strategy answered") && v.Progress == nil })
	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentAutoCompact: true, AgentFinal: "summary conversation resumed"})
	submitInteractive(t, c, view, "next", "  Next summary turn.\n")
	end := inspectInteractive(t, c, id, func(v host.View) bool { return hasFinalText(v, "summary conversation resumed") })
	calls := f.Calls(t)
	if len(calls) != 2 || claudeCallOption(calls[0], "--session-id") == "" ||
		claudeCallOption(calls[1], "--resume") != claudeCallOption(calls[0], "--session-id") ||
		calls[1].Input != "Next summary turn." {
		t.Fatal("summary strategy did not keep the previous resume path")
	}
	for _, call := range calls {
		if claudeCallOption(call, "--autocompact") != "133000" || slices.Contains(call.Environment, "DISABLE_AUTO_COMPACT=1") {
			t.Fatal("summary strategy changed CLI auto-compaction")
		}
	}
	compacted := false
	for _, item := range end.History.Items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil {
			if r.Context.Window != nil || r.Context.ConversationBinding != nil {
				t.Fatal("summary strategy recorded a three-layer window")
			}
			compacted = compacted || r.Context.Conversation != nil && r.Context.Conversation.Compacted
		}
	}
	if !compacted {
		t.Fatal("fixture did not report CLI automatic compaction")
	}
	stopMultiSession(t, c, view)
}
