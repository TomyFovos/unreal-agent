//go:build linux || darwin

package host

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestHostClaudeWindowPreservesPendingStateAndToolBatch(t *testing.T) {
	fake := testclaude.New(t)
	fake.Set(t, testclaude.Config{
		AgentSteps: []testclaude.AgentStep{{Tool: "Write", Input: jsontext.Value(`{"file_path":"approved.txt","content":"approved"}`)}},
		AgentFinal: "The approved command and child update were recorded.",
	})
	testHostWindowPending(t, fake)
}

func TestHostClaudeRotatesAtToolBatch(t *testing.T) {
	fake := testclaude.New(t)
	workspace, dir := t.TempDir(), t.TempDir()
	seedWindowHistory(t, dir)
	checkHostRestore(t, os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("sample receipt"), 0600))
	const verbatim = "  Continue through the current tools.\r\n\t続き\n"
	const final = "The task continued in the new conversation."
	fake.Set(t, testclaude.Config{
		AgentOnce: true, AgentFinal: final,
		AgentSteps: []testclaude.AgentStep{
			{Tool: "Read", Input: jsontext.Value(`{"file_path":"sample.txt"}`)},
			{Tool: "Read", Input: jsontext.Value(`{"file_path":"sample.txt"}`)},
			{Tool: "Bash", Input: jsontext.Value(`{"command":"printf 'once\\n' >> executions.log; printf 'crossed %s' result"}`)},
			{Tool: "Read", Input: jsontext.Value(`{"file_path":"unexpected-continued-call.txt"}`)},
		},
		AgentUsage: []jsontext.Value{
			jsontext.Value(`{"input_tokens":120000}`),
			jsontext.Value(`{"input_tokens":150000}`),
			jsontext.Value(`{"input_tokens":1000,"cache_read_input_tokens":198500,"cache_creation_input_tokens":500}`),
		},
	})
	ctx := t.Context()
	h, err := New(ctx, Config{Directory: dir, PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
		results := observed.NewResults()
		registry := tool.NewRegistry(tool.StaticTranslators{ClaudeCode: observed.New(results)}, tool.ClaudeCodeName)
		b, engine, err := windowBuilder(registry, contextengine.Config{})
		if err != nil {
			return Runtime{}, err
		}
		b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(claudeagent.ProviderID)
		b.(interface{ SetWindowPrompt(llm.WindowPrompt) }).SetWindowPrompt(claudeagent.WindowPromptLine)
		client, err := claudeagent.NewClient(claudeagent.Config{Binary: fake.Binary, Directory: workspace, Results: results, Namespace: dir})
		return Runtime{Builder: b, ContextEngine: engine, Tools: registry, Operations: operation.NewLocalOperationManager(ctx), LLM: client}, err
	}})
	checkHostRestore(t, err)
	t.Cleanup(func() { h.Close() })
	s, err := h.Resume(ctx, Options{ID: "window", Policy: permission.Unrestricted()})
	checkHostRestore(t, err)
	controller, err := s.PermissionControl(s.Generation)
	checkHostRestore(t, err)
	key, release := controller.AcquireControl()
	defer release()
	controlMode(t, controller, key, permission.Ask)
	_, err = s.Submit(timeout(t), s.Generation, input("work", verbatim))
	checkHostRestore(t, err)

	const approval = "claude:toolu_fake_3"
	waitCtx := timeout(t)
	for {
		snapshot, changed := controller.Watch()
		if slices.ContainsFunc(snapshot.Pending, func(p permission.Pending) bool { return p.ID == approval }) {
			break
		}
		select {
		case <-changed:
		case <-waitCtx.Done():
			t.Fatal("the crossing tool call did not reach its usual approval")
		}
	}
	path := filepath.Join(workspace, "executions.log")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("tool executed before its approval", err)
	}
	const peerText = "A child has completed its review."
	peer := inbox.PeerMessage{Version: 1, Sender: "child", Target: string(s.ID), Handle: "reviewer", Kind: "progress", Text: peerText}
	payload, err := json.Marshal(peer)
	checkHostRestore(t, err)
	_, err = s.SubmitPeer(timeout(t), s.Generation, peer.Sender, peer.Handle, inbox.Input{ID: "review", Kind: inbox.InputPeer, Payload: payload})
	checkHostRestore(t, err)
	checkHostRestore(t, controller.Decide(key, controller.Snapshot().Revision, approval, true))
	stop(t, s)

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "once\n" {
		t.Fatalf("crossing tool executions = %q, err = %v", data, err)
	}
	calls := fake.Calls(t)
	if len(calls) != 2 {
		t.Fatalf("CLI calls = %d, want one stopped turn and its continuation", len(calls))
	}
	newID := slices.Index(calls[1].Arguments, "--session-id")
	oldID := slices.Index(calls[0].Arguments, "--resume")
	if oldID < 0 || newID < 0 || calls[1].Arguments[newID+1] == calls[0].Arguments[oldID+1] || slices.Contains(calls[1].Arguments, "--resume") {
		t.Fatalf("expected --resume followed by a fresh --session-id: option indexes %d and %d, second resume %t, batch count %d",
			oldID, newID, slices.Contains(calls[1].Arguments, "--resume"), len(fake.BatchContexts(t)))
	}
	prompt := calls[1].Input
	if !strings.HasPrefix(prompt, "Unreal current state at the window boundary (recorded data): Earlier conversation is in L3.") ||
		!strings.Contains(prompt, verbatim) || !strings.Contains(prompt, "[recorded tool call] claude_code ") ||
		!strings.Contains(prompt, "[recorded tool result] ") || !strings.Contains(prompt, "crossed result") ||
		strings.Count(prompt, peerText) != 1 || strings.Contains(prompt, oldWindowInput) {
		t.Fatalf("unexpected continuation prompt: %q", prompt)
	}
	if batches := fake.BatchContexts(t); len(batches) != 3 || slices.ContainsFunc(batches, func(text string) bool { return strings.Contains(text, peerText) }) {
		t.Fatalf("the old CLI received work after its crossing batch: %q", batches)
	}
	view, err := s.Inspect(0, 4096)
	checkHostRestore(t, err)
	var receipt, boundary sessionstore.Sequence
	var continued bool
	for _, item := range view.History.Items {
		switch d := item.Data.(type) {
		case sessionstore.ToolCallStatus:
			if d.CallID == "toolu_fake_3" && len(d.Operations) == 1 && d.Operations[0].Status == operation.StatusCompleted {
				receipt = item.Sequence
			}
		case sessionstore.HostRecord:
			if d.Context != nil && d.Context.Kind == sessionstore.ContextWindowBoundary {
				w := d.Context.Window
				if boundary != 0 || w.BeforeTokens != 200_000 || w.BeforeMeasurement != "provider-reported" ||
					!slices.Equal(w.Causes, []string{"threshold", sessionstore.WindowCauseMidTurn}) {
					t.Fatalf("unexpected boundary: %+v", w)
				}
				boundary = item.Sequence
			}
		case sessionstore.ModelResponse:
			for _, out := range d.Response.Output {
				if m, ok := out.Data.(llm.Message); ok && m.Text == final {
					continued = d.WindowCache.Boundary == record.Item(string(s.ID), uint64(boundary))
				}
			}
		}
	}
	if receipt == 0 || boundary <= receipt || !continued {
		t.Fatalf("receipt=%d boundary=%d continued=%t", receipt, boundary, continued)
	}
}
