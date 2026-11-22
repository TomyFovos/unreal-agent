//go:build linux || darwin

package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeModelSwitchContinuesAtNextToolBatch(t *testing.T) {
	fake := testclaude.New(t)
	cfg := claudeRouteConfiguration(t, fake)
	workspace := cfg.Runtime.Workspace
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte("first receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	const final = "Continued the same task in the new model."
	fake.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentOnce: true, AgentFinal: final, AgentSteps: []testclaude.AgentStep{
		{Tool: "Read", Input: jsontext.Value(`{"file_path":"fixture.txt"}`)},
		{Tool: "Bash", Input: jsontext.Value(`{"command":": > started; while [ ! -f release ]; do sleep 0.01; done; printf 'once\\n' >> executions.log; printf 'crossing %s' receipt"}`)},
		{Tool: "Bash", Input: jsontext.Value(`{"command":"printf 'old turn continued\\n' >> executions.log"}`)},
	}})

	client, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
	const id = "claude-switch-at-batch"
	view, err := client.Open(t.Context(), host.Create, id)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := client.WatchPermissions(t.Context(), id, view.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Cancel()
	setGatewayHumanMode(t, watch, permission.Ask)
	submitInteractive(t, client, view, "work", "Continue through the tools.")
	approval, pending := awaitPending(t, watch)
	if pending.ID != "claude:toolu_fake_2" || len(fake.BatchContexts(t)) != 1 {
		t.Fatal("old CLI did not reach the second batch approval", pending.ID)
	}
	multiSelect(t, client, view, 0, claudeagent.ProviderID, "discovered-b", llm.ReasoningEffortHigh)
	before := inspectInteractive(t, client, id, func(v host.View) bool {
		return runtimeChoiceRecorded(v, sessionstore.HostRuntimeSelection, "discovered-b")
	})
	if runtimeChoiceRecorded(before, sessionstore.HostRuntimeApplied, "discovered-b") || len(fake.Calls(t)) != 1 {
		t.Fatal("changed runtime while the batch still needed approval")
	}
	if _, err := watch.Request(t.Context(), permission.ControlRequest{Action: "decide", Expected: approval.Revision, Operation: pending.ID, Allow: true}); err != nil {
		t.Fatal(err)
	}
	waitInteractive(t, func() bool { _, err := os.Stat(filepath.Join(workspace, "started")); return err == nil })
	const queued = "Please incorporate this queued instruction too."
	submitInteractive(t, client, view, "steer", queued)
	if len(fake.Calls(t)) != 1 || len(fake.BatchContexts(t)) != 1 {
		t.Fatal("started the continuation before the running tool finished")
	}
	if err := os.WriteFile(filepath.Join(workspace, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	end := inspectInteractive(t, client, id, func(v host.View) bool { return hasFinalText(v, final) })
	stopMultiSession(t, client, view)

	calls, batches := fake.Calls(t), fake.BatchContexts(t)
	if len(calls) != 2 || len(batches) != 2 || strings.Contains(batches[1], queued) {
		t.Fatalf("want one CLI handoff after the crossing batch, got %d calls and %v batches", len(calls), batches)
	}
	cliArgument(t, calls[0], "--model", "claude-configured-a")
	cliArgument(t, calls[1], "--model", "discovered-b")
	cliArgument(t, calls[1], "--effort", "high")
	oldID, newID := claudeCallOption(calls[0], "--session-id"), claudeCallOption(calls[1], "--session-id")
	if oldID == "" || newID == "" || newID == oldID || claudeCallOption(calls[1], "--resume") != "" {
		t.Fatal("the continuation did not open a fresh Claude conversation")
	}
	_, tail, _ := strings.Cut(calls[1].Input, "\n\n")
	for _, text := range []string{"first receipt", "crossing receipt", queued} {
		if strings.Count(tail, text) != 1 {
			t.Fatalf("expected one %q in the L1 continuation: %q", text, tail)
		}
	}
	data, err := os.ReadFile(filepath.Join(workspace, "executions.log"))
	if err != nil || string(data) != "once\n" {
		t.Fatalf("unexpected tool executions: %q (%v)", data, err)
	}
	var receipt, applied, boundary sessionstore.Sequence
	var revisions []uint64
	for _, item := range end.History.Items {
		switch r := item.Data.(type) {
		case sessionstore.ToolCallStatus:
			if r.CallID == "toolu_fake_2" && len(r.Operations) == 1 && r.Operations[0].Status == operation.StatusCompleted {
				receipt = item.Sequence
			}
		case sessionstore.HostRecord:
			if r.Kind == sessionstore.HostRuntimeApplied && r.Selection.Model == "discovered-b" {
				applied = item.Sequence
			}
			if w := r.Context; w != nil && w.Kind == sessionstore.ContextWindowBoundary {
				if boundary != 0 || w.Model != "discovered-b" || !slices.Equal(w.Window.Causes, []string{"header_change", sessionstore.WindowCauseMidTurn, sessionstore.WindowCauseSelectionChange}) {
					t.Fatalf("unexpected continuation boundary: %+v", w.Window)
				}
				boundary = item.Sequence
			}
		case session.Turn:
			revisions = append(revisions, r.RuntimeRevision)
		}
	}
	if receipt == 0 || applied <= receipt || boundary <= applied || !slices.Equal(revisions, []uint64{0, 0, 0, 1}) {
		t.Fatalf("wrong handoff order: receipt=%d applied=%d boundary=%d revisions=%v", receipt, applied, boundary, revisions)
	}
}

func TestResponsesModelSwitchUsesCompletedToolBatch(t *testing.T) {
	type wireRequest struct {
		Model       string `json:"model"`
		ServiceTier string `json:"service_tier"`
		Reasoning   struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Input jsontext.Value `json:"input"`
	}
	requests := make(chan wireRequest, 4)
	finishFirst := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := calls.Add(1)
		select {
		case requests <- req:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			select {
			case <-finishFirst:
			case <-r.Context().Done():
				t.Error("selection canceled the active Responses generation")
				return
			}
			args := `{"command":"printf 'once\\n' >> executions.log; printf 'responses %s' receipt"}`
			fmt.Fprintf(w, `data: {"type":"response.completed","response":{"id":"old","status":"completed","output":[`+
				`{"type":"function_call","call_id":"immediate","name":"unavailable","arguments":"{}"},`+
				`{"type":"function_call","call_id":"crossing","name":"Bash","arguments":%q}]}}`+"\n\n", args)
			return
		}
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"new","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Continued after tool results."}]}]}}`+"\n\n")
	}))
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	cfg := humanModeConfiguration(codexServeConfiguration(workspace, server.URL))
	client, _, _ := startInteractiveHost(t, cfg, "--codex-auth-file", writeSwitchCodexAuth(t))
	const id = "responses-switch-at-batch"
	view, err := client.Open(t.Context(), host.Create, id)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := client.WatchPermissions(t.Context(), id, view.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Cancel()
	setGatewayHumanMode(t, watch, permission.Ask)
	awaitRequest := func() wireRequest {
		t.Helper()
		select {
		case req := <-requests:
			return req
		case <-time.After(5 * time.Second):
			t.Fatal("no fake Responses request")
			return wireRequest{}
		}
	}
	submitInteractive(t, client, view, "work", "Use the tools and continue.")
	if req := awaitRequest(); req.Model != cfg.Runtime.Provider.Model.ID || req.ServiceTier != "" {
		t.Fatal("wrong initial request", req.Model, req.ServiceTier)
	}
	choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "fast-switch", Model: "model-b", Effort: llm.ReasoningEffortHigh, Fast: true}
	if _, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.select", ID: id, Generation: view.Generation, Selection: choice}); err != nil {
		t.Fatal(err)
	}
	close(finishFirst)
	approval, pending := awaitPending(t, watch)
	before := inspectInteractive(t, client, id, func(v host.View) bool {
		return runtimeChoiceRecorded(v, sessionstore.HostRuntimeSelection, "model-b")
	})
	if runtimeChoiceRecorded(before, sessionstore.HostRuntimeApplied, "model-b") || calls.Load() != 1 {
		t.Fatal("started a continuation for a partial Responses tool batch")
	}
	if _, err := watch.Request(t.Context(), permission.ControlRequest{Action: "decide", Expected: approval.Revision, Operation: pending.ID, Allow: true}); err != nil {
		t.Fatal(err)
	}
	if req := awaitRequest(); req.Model != "model-b" || req.Reasoning.Effort != "high" || req.ServiceTier != "priority" ||
		strings.Count(string(req.Input), "responses receipt") != 1 || !strings.Contains(string(req.Input), "unavailable") {
		t.Fatalf("continuation lost its selection or receipts: %+v", req)
	}
	inspectInteractive(t, client, id, func(v host.View) bool { return hasFinalText(v, "Continued after tool results.") })
	stopMultiSession(t, client, view)
	data, err := os.ReadFile(filepath.Join(workspace, "executions.log"))
	if calls.Load() != 2 || err != nil || string(data) != "once\n" {
		t.Fatalf("unexpected requests or tool executions: %d calls, %q (%v)", calls.Load(), data, err)
	}
}

func TestClaudeModelSwitchReleasesUsageLimitWait(t *testing.T) {
	fake := testclaude.New(t)
	reset := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	fake.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, Exit: 1, AgentReply: []jsontext.Value{
		jsontext.Value(fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":%d}}`, reset.Unix())),
		jsontext.Value(`{"type":"result","subtype":"success","is_error":true,"api_error":"usage_limit_reached"}`),
	}})
	client, _, _ := startInteractiveHost(t, claudeRouteConfiguration(t, fake), "--normal-runtime")
	const id = "claude-switch-in-wait"
	view, err := client.Open(t.Context(), host.Create, id)
	if err != nil {
		t.Fatal(err)
	}
	const prompt = "Finish the request after the quota wait."
	submitInteractive(t, client, view, "work", prompt)
	waiting := func(v host.View) bool {
		return v.Progress != nil && v.Progress.Mode == "usage_limit_wait" && v.Progress.RetryAt.Equal(reset)
	}
	inspectInteractive(t, client, id, waiting)
	multiSelect(t, client, view, 0, claudeagent.ProviderID, "claude-configured-a", llm.ReasoningEffortMedium)
	same := inspectInteractive(t, client, id, func(v host.View) bool {
		return runtimeChoiceRecorded(v, sessionstore.HostRuntimeSelection, "claude-configured-a")
	})
	if !waiting(same) || runtimeChoiceRecorded(same, sessionstore.HostRuntimeApplied, "claude-configured-a") || len(fake.Calls(t)) != 1 {
		t.Fatal("an effort-only change ended the usage-limit wait")
	}
	const final = "Resumed with a different model."
	fake.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: final})
	multiSelect(t, client, view, 1, claudeagent.ProviderID, "discovered-b", llm.ReasoningEffortHigh)
	end := inspectInteractive(t, client, id, func(v host.View) bool { return hasFinalText(v, final) && v.Progress == nil })
	stopMultiSession(t, client, view)
	calls := fake.Calls(t)
	if len(calls) != 2 || !runtimeChoiceRecorded(end, sessionstore.HostRuntimeApplied, "discovered-b") {
		t.Fatalf("selection did not resume the waiting request exactly once: %d CLI calls", len(calls))
	}
	cliArgument(t, calls[1], "--model", "discovered-b")
	_, tail, _ := strings.Cut(calls[1].Input, "\n\n")
	if strings.Count(tail, prompt) != 1 {
		t.Fatalf("expected one waiting request in the L1 continuation: %q", tail)
	}
}

func TestResponsesModelSwitchReleasesUsageLimitWait(t *testing.T) {
	reset := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const prompt = "Finish the Responses request after the quota wait."
	const final = "Resumed the Responses request."
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req multiWire
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if calls.Add(1) == 1 {
			if req.Model != "gpt-6.1-sol" {
				t.Error("unexpected model on the first Responses request", req.Model)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":{"type":"usage_limit_reached","resets_at":%d}}`, reset.Unix())
			return
		}
		input, err := json.Marshal(req.Input)
		if err != nil || req.Model != "model-b" || !strings.Contains(string(input), prompt) {
			t.Error("retry lost the new model or original input", req.Model, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"type":"response.completed","response":{"id":"new","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}}`+"\n\n", final)
	}))
	t.Cleanup(server.Close)
	client, _, _ := startInteractiveHost(t, codexServeConfiguration(t.TempDir(), server.URL), "--codex-auth-file", writeSwitchCodexAuth(t))
	const id = "responses-switch-in-wait"
	view, err := client.Open(t.Context(), host.Create, id)
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, client, view, "work", prompt)
	inspectInteractive(t, client, id, func(v host.View) bool {
		return v.Progress != nil && v.Progress.Mode == "usage_limit_wait" && v.Progress.RetryAt.Equal(reset)
	})
	multiSelect(t, client, view, 0, "openai-codex", "model-b", llm.ReasoningEffortHigh)
	inspectInteractive(t, client, id, func(v host.View) bool { return hasFinalText(v, final) && v.Progress == nil })
	stopMultiSession(t, client, view)
	if calls.Load() != 2 {
		t.Fatalf("expected one retry on model-b, got %d total calls", calls.Load())
	}
}

func writeSwitchCodexAuth(t *testing.T) string {
	t.Helper()
	dir := privateCLIDirectory(t)
	auth := filepath.Join(dir, "auth.json")
	writeCodexAuth(t, auth, "switch-token-sensitive", "switch-account-sensitive")
	catalog := []byte(`{"models":[{"slug":"model-b","display_name":"Model B","visibility":"list","supported_reasoning_levels":[{"effort":"high"}],"default_reasoning_level":"high","context_window":272000,"service_tiers":[{"id":"priority","name":"Fast"}]}]}`)
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), catalog, 0600); err != nil {
		t.Fatal(err)
	}
	return auth
}

func runtimeChoiceRecorded(view host.View, kind, model string) bool {
	return slices.ContainsFunc(view.History.Items, func(item host.HistoryItem) bool {
		r, ok := item.Data.(sessionstore.HostRecord)
		return ok && r.Kind == kind && r.Selection != nil && r.Selection.Model == model
	})
}
