package contextbuilder

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func epochCall(id, name, arguments string) llm.Item {
	return llm.Item{Type: llm.ItemToolCall, ProviderID: "fc-" + id, Data: llm.ToolCall{CallID: id, Name: name, Arguments: arguments}}
}

func epochApply(t testing.TB, f *contextFixture, kind sessionstore.ItemKind, data any) {
	t.Helper()
	status, receipt := data.(sessionstore.ToolCallStatus)
	record, contextRecord := data.(sessionstore.HostRecord)
	if !receipt && !contextRecord {
		f.item(t, kind, data)
		return
	}
	f.sequence++
	item := sessionstore.Item{Sequence: f.sequence, Kind: kind, Data: data}
	f.b.SetHistoryItem(item)
	if receipt {
		state, err := operation.DecodeShellState(status.Operations[0])
		if err != nil {
			t.Fatal(err)
		}
		running := status.Operations[0].Status == operation.StatusAwaiting
		text := "running"
		if state.Result != nil {
			text = state.Result.Out
		}
		f.b.AddTypedToolResult(llm.ToolResult{CallID: status.CallID, State: llm.ToolResultSucceeded, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}, running)
	} else if record.Context != nil {
		f.b.ApplyContext(*record.Context)
	}
	f.b.ObserveHistory(item)
	f.history = append(f.history, item)
}

func epochReceipt(t testing.TB, f *contextFixture, id, text string, running bool) {
	t.Helper()
	state := operation.ShellState{Input: operation.ShellInput{Shell: "/bin/bash", Command: "cat source.go", Directory: "/workspace"}, BaseDirectory: "/captures", OutPath: "/captures/" + id + "/out", ErrPath: "/captures/" + id + "/err", Result: &operation.ShellResult{Out: text}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: operation.ID("op-" + id), Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusCompleted, MaxOutputLength: 40000, State: raw}
	if running {
		op.Status = operation.StatusAwaiting
	}
	status := sessionstore.ToolCallStatus{TurnID: session.TurnID(id), CallID: id, Operations: []operation.Operation{op}}
	status.Status.WaitingFor = []operation.ID{op.ID}
	epochApply(t, f, sessionstore.ItemToolCallStatus, status)
}

func epochExchange(t testing.TB, f *contextFixture, id, name, args, output string) {
	t.Helper()
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id), Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(id), Response: llm.Response{Output: []llm.Item{epochCall(id, name, args)}}})
	epochReceipt(t, f, id, output, false)
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: session.TurnID(id + "-ack"), Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: session.TurnID(id + "-ack"), Response: llm.Response{}})
}

func epochPrepare(t testing.TB, f *contextFixture) []sessionstore.ContextRecord {
	t.Helper()
	var records []sessionstore.ContextRecord
	err := f.b.PrepareContext(t.Context(), nil, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error {
		records = append(records, r)
		epochApply(t, f, sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &r})
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func epochPrunes(records []sessionstore.ContextRecord) []sessionstore.ContextRecord {
	var prunes []sessionstore.ContextRecord
	for _, r := range records {
		if r.Kind == "prune" {
			prunes = append(prunes, r)
		}
	}
	return prunes
}

func TestToolEpochWaitsThenPrunesAllOldResultsAtOnce(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 1000, ToolResultMinPruneTokens: 3000})
	f.user(t, "task", "Keep the task")
	for i := range 3 {
		id := fmt.Sprint(i)
		epochExchange(t, f, id, "Bash", `{"command":"printf unknown"}`, strings.Repeat("x", 2200))
		prunes := epochPrunes(epochPrepare(t, f))
		if i < 2 {
			if len(prunes) != 0 || len(f.b.pruned) != 0 {
				t.Fatal("pruned below minimum", i)
			}
		} else if len(prunes) != 1 || len(prunes[0].Projections) != 2 || prunes[0].PrunedTokens < 3000 {
			t.Fatalf("not one complete epoch: %+v", prunes)
		}
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range built.Request.Input {
		if r, ok := item.Data.(llm.ToolResult); ok {
			omitted := strings.Contains(r.Output[0].Value, "Output omitted")
			if omitted != (r.CallID != "2") {
				t.Fatal("recent/old boundary wrong", r.CallID)
			}
			if omitted && (!strings.Contains(r.Output[0].Value, "/captures/"+r.CallID+"/out") || strings.Contains(r.Output[0].Value, "\n")) {
				t.Fatal("omission lost capture path or single line")
			}
		}
	}
	if len(epochPrunes(epochPrepare(t, f))) != 0 {
		t.Fatal("same results pruned twice")
	}
}

func TestToolEpochMinimumIsInclusiveAndUsesNetSavings(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 1, ToolResultMinPruneTokens: 100000})
	f.user(t, "task", "Inspect")
	epochExchange(t, f, "old", "read", `{"path":"old.go"}`, strings.Repeat("x", 2000))
	epochExchange(t, f, "new", "read", `{"path":"new.go"}`, strings.Repeat("y", 2000))
	var saved int64
	for _, u := range f.b.engine.Units() {
		if r, ok := u.Item.Data.(llm.ToolResult); ok && r.CallID == "old" {
			before := contextengine.Estimate(r)
			r.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: f.b.omittedOutput(u, r, f.b.toolCalls()[r.CallID])}}
			saved = before - contextengine.Estimate(r)
		}
	}
	if saved <= 0 {
		t.Fatal("fixture has no savings")
	}
	f.b.contextConfig.ToolResultMinPruneTokens = saved + 1
	if len(epochPrunes(epochPrepare(t, f))) != 0 {
		t.Fatal("gross result size triggered pruning")
	}
	f.b.contextConfig.ToolResultMinPruneTokens = saved
	if records := epochPrunes(epochPrepare(t, f)); len(records) != 1 || records[0].PrunedTokens != saved {
		t.Fatal("minimum equality did not prune", records)
	}
}

func TestToolEpochObsoleteReadsInsideRecentWindow(t *testing.T) {
	for _, old := range []struct{ name, args string }{{"read", `{"path":"src/foo.go","start_line":1,"lines":10}`}, {"Bash", `{"command":"sed -n '1,200p' src/foo.go"}`}} {
		for _, later := range []struct {
			name, args string
			obsolete   bool
		}{
			{"read", `{"path":"./src/foo.go","start_line":200}`, true},
			{"Bash", `{"command":"sed -n '201,300p' src/foo.go"}`, true},
			{"edit", `{"path":"src/foo.go"}`, true},
			{"write", `{"path":"src/foo.go"}`, true},
			{"ast_edit", `{"targets":[{"path":"src/foo.go"}],"apply":true}`, true},
			{"ast_edit", `{"targets":[{"path":"src/foo.go"}],"apply":false}`, false},
			{"Bash", `{"command":"cat src/foo.go other.go"}`, false},
		} {
			t.Run(old.name+"/"+later.name+later.args, func(t *testing.T) {
				f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 100000, ToolResultMinPruneTokens: 1000})
				f.user(t, "task", "Inspect")
				epochExchange(t, f, "old", old.name, old.args, strings.Repeat("old\n", 1000))
				epochExchange(t, f, "unknown", "Bash", `{"command":"cat src/foo.go other.go"}`, strings.Repeat("unclassified\n", 300))
				f.item(t, sessionstore.ItemTurn, session.Turn{ID: "later", Type: session.TurnRegular})
				f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "later", Response: llm.Response{Output: []llm.Item{epochCall("later", later.name, later.args)}}})
				epochReceipt(t, f, "later", "new snapshot", false)
				prunes := epochPrunes(epochPrepare(t, f))
				if (len(prunes) == 1) != later.obsolete {
					t.Fatal("obsolete detection wrong", prunes)
				}
				if later.obsolete && len(prunes[0].Projections) != 1 {
					t.Fatal("unclassified Bash or latest result pruned")
				}
			})
		}
	}
}

func TestSimpleReadPathConservativeShellGrammar(t *testing.T) {
	for _, command := range []string{"cat src/a.go", "sed -n '1,200p' src/a.go", "head src/a.go", "head -n 10 src/a.go", "tail -n 10 src/a.go", "nl -ba src/a.go", "nl -ba src/a.go | sed -n '1,20p'", "cat src/a.go|sed -n '1,20p'", `cat 'src/a.go'`} {
		if got := simpleReadPath(command); got != "src/a.go" {
			t.Errorf("%q = %q", command, got)
		}
	}
	for _, command := range []string{"cat a b", "cat *.go", "cat $FILE", "cat $(pwd)/a", "cat a; cat b", "cat a && true", "cat a >b", "cat a | head", "cat a | sed -n '1p' | sed -n '2p'", "sed -i 's/a/b/' a", "head -c 10 a", "cat -", "cat 'unterminated", "cat a\ncat b", "cat a || cat b"} {
		if got := simpleReadPath(command); got != "" {
			t.Errorf("classified ambiguous %q as %q", command, got)
		}
	}
}

func TestToolEpochProtectsUnreceivedRunningReasoningAndMessages(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 1, ToolResultMinPruneTokens: 1000})
	f.user(t, "task", "User task preserved")
	epochExchange(t, f, "old", "read", `{"path":"old.go"}`, strings.Repeat("old", 2000))
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "unreceived", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "unreceived", Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"type":"reasoning","encrypted_content":"keep-private"}`)}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Keep assistant speech"}},
		epochCall("unreceived", "read", `{"path":"next.go"}`), epochCall("running", "Bash", `{"command":"cat running.go"}`),
	}}})
	epochReceipt(t, f, "unreceived", strings.Repeat("unreceived", 1000), false)
	epochReceipt(t, f, "running", "running", true)
	// Materialize the running placeholder without acknowledging the terminal
	// sibling, just as PrepareContext does before the next model request.
	if len(epochPrunes(epochPrepare(t, f))) != 1 {
		t.Fatal("older independent result not pruned")
	}
	for _, u := range f.b.engine.Units() {
		if r, ok := u.Item.Data.(llm.ToolResult); ok && r.CallID != "old" && f.b.pruned[u.ID] {
			t.Fatal("required receipt pruned", r.CallID)
		}
	}
	raw, _ := json.Marshal(f.b.deriveItems())
	for _, want := range []string{"keep-private", "User task preserved", "Keep assistant speech", strings.Repeat("unreceived", 1000)} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatal("protected content lost", want[:min(len(want), 30)])
		}
	}
}

func TestToolEpochPersistFailureDoesNotChangeView(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 1, ToolResultMinPruneTokens: 1000})
	f.user(t, "task", "Inspect")
	epochExchange(t, f, "old", "read", `{"path":"old.go"}`, strings.Repeat("x", 3000))
	epochExchange(t, f, "new", "read", `{"path":"new.go"}`, strings.Repeat("y", 3000))
	before, _ := json.Marshal(f.b.deriveItems())
	failure := errors.New("storage failed")
	err := f.b.pruneToolEpoch(func(sessionstore.ContextRecord) error { return failure })
	after, _ := json.Marshal(f.b.deriveItems())
	if !errors.Is(err, failure) || !bytes.Equal(before, after) || len(f.b.pruned) != 0 {
		t.Fatal("failed persistence changed view", err)
	}
}

func TestToolEpochUsesRecordedMutationTargetsAndProtectsActiveOperations(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 100000, ToolResultMinPruneTokens: 1000})
	f.user(t, "task", "Inspect")
	epochExchange(t, f, "changed", "read", `{"path":"src/foo.go"}`, strings.Repeat("old", 2000))
	epochExchange(t, f, "active", "read", `{"path":"src/active.go"}`, strings.Repeat("active", 1000))
	// An active Operation also protects a receipt whose continuation has already
	// been acknowledged (for example an earlier status snapshot).
	f.b.ObserveOperation(operation.Operation{ID: "op-active", Status: operation.StatusAwaiting})
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "mutate", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "mutate", Response: llm.Response{Output: []llm.Item{epochCall("mutate", "custom_change", `{}`)}}})
	state := operation.RemoteJobState{Plan: operation.RemoteJobPlan{Type: "structural_code", Version: 1, Data: []byte(`{}`)}, Handle: []byte(`{"code":"failed","mutation":{"targets":[{"path":"src/foo.go","code":"applied"},{"path":"src/active.go","code":"applied"}]}}`)}
	raw, _ := json.Marshal(state)
	status := sessionstore.ToolCallStatus{TurnID: "mutate", CallID: "mutate", Operations: []operation.Operation{{ID: "mutation-op", Type: operation.TypeRemoteJob, Version: operation.VersionRemoteJob, Status: operation.StatusFailed, MaxOutputLength: 40000, State: raw}}}
	f.sequence++
	item := sessionstore.Item{Sequence: f.sequence, Kind: sessionstore.ItemToolCallStatus, Data: status}
	f.b.SetHistoryItem(item)
	f.b.AddTypedToolResult(llm.ToolResult{CallID: "mutate", State: llm.ToolResultFailed, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Partially applied"}}}, false)
	f.b.ObserveHistory(item)
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "ack", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "ack", Response: llm.Response{}})
	prunes := epochPrunes(epochPrepare(t, f))
	if len(prunes) != 1 || len(prunes[0].Projections) != 1 {
		t.Fatal("changed targets or active protection wrong", prunes)
	}
	u, _ := f.b.engine.ResolveUnit(prunes[0].Projections[0].UnitID)
	if u.Item.Data.(llm.ToolResult).CallID != "changed" {
		t.Fatal("active receipt was pruned")
	}
}

func TestToolEpochFailedRereadAndClaudeDoNotPrune(t *testing.T) {
	for _, provider := range []string{"openai-codex", "claude-code"} {
		f := newContextFixture(t, provider, contextengine.Config{ToolResultKeepTokens: 100000, ToolResultMinPruneTokens: 1000})
		f.user(t, "task", "Inspect")
		epochExchange(t, f, "old", "read", `{"path":"source.go"}`, strings.Repeat("old", 2000))
		epochExchange(t, f, "failed", "read", `{"path":"source.go"}`, "Read failed")
		for _, u := range f.b.engine.Units() {
			if r, ok := u.Item.Data.(llm.ToolResult); ok && r.CallID == "failed" {
				info := f.b.receiptInfo[u.ID]
				info.failed = true
				f.b.receiptInfo[u.ID] = info
			}
		}
		if provider == "claude-code" {
			f.b.contextConfig.ToolResultKeepTokens = 1
		}
		if len(epochPrunes(epochPrepare(t, f))) != 0 {
			t.Fatal("failed reread/Claude pruned", provider)
		}
	}
}

func TestToolEpochCanonicalStoreReplayAndLegacyPrune(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{ToolResultKeepTokens: 1, ToolResultMinPruneTokens: 1000})
	f.user(t, "task", "Inspect")
	for i := range 3 {
		epochExchange(t, f, fmt.Sprint(i), "read", fmt.Sprintf(`{"path":"file%d.go"}`, i), strings.Repeat("output", 600))
	}
	epochPrepare(t, f)
	dir := t.TempDir()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	var previous session.TurnID
	for _, item := range f.history {
		switch v := item.Data.(type) {
		case inbox.Input:
			err = store.AppendInput(t.Context(), "session", v)
		case session.Turn:
			v.PreviousTurnID = previous
			previous = v.ID
			err = store.AppendTurn(t.Context(), "session", v)
		case sessionstore.ModelResponse:
			err = store.AppendModelResponse(t.Context(), "session", v)
		case sessionstore.ToolCallStatus:
			err = store.AppendToolCallStatus(t.Context(), "session", v)
		case sessionstore.HostRecord:
			err = store.AppendHostRecord(t.Context(), "session", v)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err = localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, items, _, err := store.ReadSnapshot(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	replay := newContextFixture(t, "openai-codex", f.b.contextConfig)
	for _, item := range items {
		epochApply(t, replay, item.Kind, item.Data)
	}
	before, _ := json.Marshal(f.b.deriveItems())
	after, _ := json.Marshal(replay.b.deriveItems())
	if !bytes.Equal(before, after) || !reflect.DeepEqual(f.b.pruned, replay.b.pruned) {
		t.Fatal("durable replay changed view")
	}
	if len(epochPrunes(epochPrepare(t, replay))) != 0 {
		t.Fatal("resume re-pruned old results")
	}
	// The original single-unit prune encoding still restores its body and state.
	for _, u := range replay.b.engine.Units() {
		if r, ok := u.Item.Data.(llm.ToolResult); ok && r.CallID == "2" {
			r.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "legacy reference"}}
			replay.b.ApplyContext(sessionstore.ContextRecord{Kind: "prune", UnitID: u.ID, Items: []llm.Item{{Type: llm.ItemToolResult, Data: r}}})
		}
	}
	legacy, _ := json.Marshal(replay.b.deriveItems())
	if !bytes.Contains(legacy, []byte("legacy reference")) {
		t.Fatal("old prune unreadable")
	}
}

func TestToolEpochLongConversationReducesAverageInput(t *testing.T) {
	config := contextengine.Config{FallbackWindow: 4000000, InputBudget: 4000000}
	baseline := newContextFixture(t, "openai-codex", config)
	compacted := newContextFixture(t, "openai-codex", config)
	baseline.b.contextConfig.ToolResultMinPruneTokens = 4000000
	var totals [2]int64
	var epochs int
	for index, f := range []*contextFixture{baseline, compacted} {
		f.user(t, "task", "Inspect a long synthetic conversation")
		for i := range 120 {
			id := fmt.Sprint(i)
			epochExchange(t, f, id, "read", fmt.Sprintf(`{"path":"source%d.go"}`, i), strings.Repeat("source line\n", 400))
			// Synthetic measured usage is a quarter of the conservative JSON
			// estimate. This exercises the same calibration as real Responses.
			request := llm.Request{Model: f.b.request.Model, Tools: f.b.request.Tools, Input: f.b.deriveItems()}
			input := contextengine.Estimate(request) / 4
			turn := session.TurnID(id + "-measured")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: turn, Type: session.TurnRegular})
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{Usage: llm.Usage{InputTokens: input}}})
			prunes := epochPrunes(epochPrepare(t, f))
			if index == 1 {
				epochs += len(prunes)
			}
			built, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			totals[index] += contextengine.Estimate(built.Request) / 4
		}
	}
	generatedTokens := 120 * len(strings.Repeat("source line\n", 400)) / 4
	if generatedTokens < 100000 {
		t.Fatal("fixture has fewer than 100k result tokens")
	}
	if totals[1] >= totals[0]*7/10 || epochs == 0 {
		t.Fatal("average input did not fall substantially", totals, epochs)
	}
	t.Logf("120 calls, %d generated result tokens; average input %.0f -> %.0f (%.1f%% reduction), %d epochs", generatedTokens, float64(totals[0])/120, float64(totals[1])/120, 100*(1-float64(totals[1])/float64(totals[0])), epochs)
}
