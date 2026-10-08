//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

type multiWire struct {
	Model        string `json:"model"`
	Tools        []any  `json:"tools"`
	Input        []any  `json:"input"`
	Instructions string `json:"instructions"`
}

func TestMultiProviderCodexToolLoopFinishesBeforeClaudeCapabilitySwitch(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog})
	requests := make(chan multiWire, 4)
	release := make(chan struct{}, 1)
	var calls atomic.Int32
	var target string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request multiWire
		if e := json.UnmarshalRead(r.Body, &request); e != nil {
			t.Error(e)
			return
		}
		requests <- request
		var output []any
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			args, _ := json.Marshal(map[string]string{"path": target})
			output = []any{
				map[string]any{"type": "reasoning", "id": "native-reasoning", "summary": []any{}, "encrypted_content": "PRIVATE_CODEX_STATE"},
				map[string]any{"type": "function_call", "id": "native-read", "call_id": "native-read", "name": "read", "arguments": string(args)},
			}
		} else {
			output = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Codex loop completed"}}}}
		}
		body, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("codex-", calls.Load()), "model": "observed-codex", "status": "completed", "output": output}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", body)
	}))
	t.Cleanup(server.Close)
	cfg := multiConfiguration(t, f, server.URL, true)
	cfg.Permissions.Tools = []string{"read"}
	target = filepath.Join(cfg.Runtime.Workspace, "receipt.txt")
	if e := os.WriteFile(target, []byte("NATIVE_READ_RECEIPT"), 0600); e != nil {
		t.Fatal(e)
	}
	c, _, _ := startClaudeStreamHost(t, cfg, privateCLIDirectory(t))
	v, e := c.Open(t.Context(), host.Create, "tool-loop")
	if e != nil {
		t.Fatal(e)
	}
	submitInteractive(t, c, v, "with-tool", "read the fixture")
	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("no first Codex request")
	}
	multiSelect(t, c, v, 0, "claude-code", "claude-configured-a", "medium")
	release <- struct{}{}
	select {
	case wire := <-requests:
		input, _ := json.Marshal(wire.Input)
		if len(wire.Tools) == 0 || !strings.Contains(string(input), "function_call_output") || !strings.Contains(string(input), "NATIVE_READ_RECEIPT") || !strings.Contains(string(input), "PRIVATE_CODEX_STATE") {
			t.Fatal("Codex native translator/tool context changed before boundary")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Codex tool continuation did not execute")
	}
	done := inspectInteractive(t, c, "tool-loop", func(v host.View) bool { return responseCount(v) == 2 })
	if len(f.Calls(t)) != 0 || len(done.Operations) != 1 || done.Operations[0].ToolName != "read" || done.Operations[0].Status != operation.StatusCompleted {
		t.Fatal("pending switch changed Codex operation ownership")
	}
	submitInteractive(t, c, v, "as-claude", "reply as text")
	final := inspectInteractive(t, c, "tool-loop", func(v host.View) bool { return responseCount(v) == 3 })
	cli := f.Calls(t)
	if len(cli) != 1 || !strings.Contains(cli[0].Input, "NATIVE_READ_RECEIPT") || strings.Contains(cli[0].Input, "PRIVATE_CODEX_STATE") {
		t.Fatal("Claude lost portable receipt or received private Codex state")
	}
	cliArgument(t, cli[0], "--tools", "")
	report := multiReport(t, final)
	if len(final.Operations) != 1 || report.Turns[0].Selection.Provider != "openai-codex" || report.Turns[1].Selection.Provider != "openai-codex" || report.Turns[2].Selection.Provider != "claude-code" || report.Turns[2].Selection.Revision != 1 {
		t.Fatal("tool loop or prior turn metadata changed")
	}
}

func multiConfiguration(t *testing.T, f *testclaude.Fixture, endpoint string, codexFirst bool) serveConfiguration {
	t.Helper()
	c := claudeServeConfiguration(t, f)
	c.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	codex := codexServeConfiguration(c.Runtime.Workspace, endpoint).Runtime
	if codexFirst {
		p := agentrunner.ProviderRuntime{Provider: c.Runtime.Provider, ReasoningEffort: c.Runtime.ReasoningEffort, ClaudeCode: c.Runtime.ClaudeCode}
		c.Runtime = codex
		c.Providers = map[string]agentrunner.ProviderRuntime{"claude-code": p}
	} else {
		c.Providers = map[string]agentrunner.ProviderRuntime{"openai-codex": {Provider: codex.Provider, ReasoningEffort: codex.ReasoningEffort}}
	}
	auth := filepath.Join(f.Home, ".codex")
	if e := os.MkdirAll(auth, 0700); e != nil {
		t.Fatal(e)
	}
	writeCodexAuth(t, filepath.Join(auth, "auth.json"), "multi-token-sensitive", "multi-account-sensitive")
	cache := `{"models":[{"slug":"gpt-6.1-sol","display_name":"Synthetic GPT","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`
	if e := os.WriteFile(filepath.Join(auth, "models_cache.json"), []byte(cache), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("CODEX_HOME", auth)
	t.Setenv("OPENAI_CODEX_AUTH_FILE", "")
	return c
}
func multiSelect(t *testing.T, c *gateway.Client, v host.View, revision uint64, provider, model string, effort llm.ReasoningEffort) {
	t.Helper()
	_, e := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: v.Session.Session.ID, Generation: v.Generation, Expected: revision, Selection: sessionstore.RuntimeSelection{Version: 1, Provider: provider, Model: model, Effort: effort, RequestID: fmt.Sprintf("select-%d-%s", revision, provider)}})
	if e != nil {
		t.Fatal("selection", e)
	}
}
func multiReport(t *testing.T, v host.View) analysis.Report {
	t.Helper()
	a := analysis.New(v.Session.Session.ID)
	for _, item := range v.History.Items {
		a.Apply(item)
	}
	return a.Snapshot(time.Now(), !v.History.More, false, nil, v.Operations)
}
func stopMultiSession(t *testing.T, c *gateway.Client, v host.View) {
	t.Helper()
	if _, e := c.Stop(t.Context(), v.Session.Session.ID, v.Generation, inbox.StopWhenIdle, "test"); e != nil {
		t.Fatal(e)
	}
	inspectInteractive(t, c, v.Session.Session.ID, func(v host.View) bool { return !v.Running })
}

func TestMultiProviderBoundaryCapabilitiesNamespaceAnalysisAndResume(t *testing.T) {
	f := testclaude.New(t)
	gate := filepath.Join(f.Directory, "release")
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, Gate: gate})
	requests := make(chan multiWire, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer multi-token-sensitive" {
			t.Error("external Codex source lost")
		}
		var wire multiWire
		if e := json.UnmarshalRead(r.Body, &wire); e != nil {
			t.Error(e)
		}
		requests <- wire
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"codex-response","model":"wire-codex","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic Codex response"}]}],"usage":{"input_tokens":20,"output_tokens":3}}}`+"\n\n")
	}))
	defer server.Close()
	cfg := multiConfiguration(t, f, server.URL, false)
	directory := privateCLIDirectory(t)
	c, shutdown, output := startClaudeStreamHost(t, cfg, directory)
	v, e := c.Open(t.Context(), host.Create, "multi-main")
	if e != nil {
		t.Fatal(e)
	}
	out, e := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.providers", ID: "multi-main"})
	if e != nil || len(out.Providers) != 2 {
		t.Fatal("missing providers", e)
	}
	for _, p := range out.Providers {
		if p.Availability != "available" {
			t.Fatal("available provider unavailable", p.ID)
		}
		if p.ID == "claude-code" && (p.Tools || len(p.Catalog.Models) != 3) {
			t.Fatal("Claude namespace/capability")
		}
		if p.ID == "openai-codex" && (!p.Tools || len(p.Catalog.Models) != 1) {
			t.Fatal("Codex namespace/capability")
		}
	}
	submitInteractive(t, c, v, "first", "first private user body")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 1 })
	multiSelect(t, c, v, 0, "openai-codex", "gpt-6.1-sol", "medium")
	busy, _ := c.Inspect(t.Context(), "multi-main", 0, 256)
	for _, item := range busy.History.Items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Kind == sessionstore.HostRuntimeApplied {
			t.Fatal("provider changed midstream")
		}
	}
	if e = os.WriteFile(gate, []byte("go"), 0600); e != nil {
		t.Fatal(e)
	}
	inspectInteractive(t, c, "multi-main", func(v host.View) bool { return responseCount(v) == 1 })
	submitInteractive(t, c, v, "second", "second private user body")
	select {
	case wire := <-requests:
		if wire.Model != "gpt-6.1-sol" || len(wire.Tools) == 0 {
			t.Fatal("Codex tools not restored")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Codex request")
	}
	inspectInteractive(t, c, "multi-main", func(v host.View) bool { return responseCount(v) == 2 })
	multiSelect(t, c, v, 1, "claude-code", "discovered-b", "high")
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, Stream: strings.ReplaceAll(textMetadataStream, "wire-a", "wire-b")})
	submitInteractive(t, c, v, "third", "third private user body")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 2 })
	cliArgument(t, f.Calls(t)[1], "--model", "discovered-b")
	cliArgument(t, f.Calls(t)[1], "--effort", "high")
	cliArgument(t, f.Calls(t)[1], "--tools", "")
	done := inspectInteractive(t, c, "multi-main", func(v host.View) bool { return responseCount(v) == 3 })
	if len(done.Operations) != 0 {
		t.Fatal("text smoke created Tool Operations")
	}
	report := multiReport(t, done)
	if report.Selection.Provider != "claude-code" || report.Selection.Revision != 2 || report.ToolCapability != "text-only" || len(report.Turns) != 3 {
		t.Fatal("current projection", report.Selection)
	}
	for i, want := range []string{"claude-code", "openai-codex", "claude-code"} {
		if report.Turns[i].Selection.Provider != want || report.Turns[i].Selection.Revision != uint64(i) {
			t.Fatal("prior turn rewritten", i)
		}
	}
	if report.Turns[0].ObservedModel == "" || report.Turns[2].ObservedModel != "wire-b" || report.Turns[2].Selection.Model != "discovered-b" {
		t.Fatal("selection/observation conflated")
	}
	for _, format := range []string{"JSON", "Markdown"} {
		b, e := analysis.Encode(report, format)
		if e != nil {
			t.Fatal(e)
		}
		assertNoCredentialLeak(t, string(b), "multi-token-sensitive", "multi-account-sensitive", "first private user body", "third private user body")
		if !strings.Contains(string(b), "openai-codex") || !strings.Contains(string(b), "claude-code") {
			t.Fatal("provider missing from export")
		}
	}
	multiSelect(t, c, v, 2, "openai-codex", "gpt-6.1-sol", "high")
	stopMultiSession(t, c, v)
	shutdown()
	c2, _, _ := startClaudeStreamHost(t, cfg, directory)
	resumed, e := c2.Open(t.Context(), host.Resume, "multi-main")
	if e != nil {
		t.Fatal(e)
	}
	r := multiReport(t, resumed)
	if r.Selection.Provider != "claude-code" || r.Pending == nil || r.Pending.Provider != "openai-codex" || r.Pending.Revision != 3 {
		t.Fatal("resume lost applied/pending provider", r.Selection, r.Pending)
	}
	submitInteractive(t, c2, resumed, "after-restart", "final user body")
	select {
	case wire := <-requests:
		if len(wire.Tools) == 0 {
			t.Fatal("resume lost Codex tools")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resumed request")
	}
	final := inspectInteractive(t, c2, "multi-main", func(v host.View) bool { return responseCount(v) == 4 })
	if r = multiReport(t, final); r.Selection.Provider != "openai-codex" || r.Selection.Revision != 3 || r.ToolCapability != "tools supported" {
		t.Fatal("resume selection")
	}
	assertNoCredentialLeak(t, output.String(), "multi-token-sensitive", "multi-account-sensitive")
}

func TestMultiProviderUnavailableBackendNoFallbackAndSingleSessionUpgrade(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`+"\n\n")
	}))
	defer server.Close()
	cfg := multiConfiguration(t, f, server.URL, false)
	extra := cfg.Providers
	cfg.Providers = nil
	directory := privateCLIDirectory(t)
	c, stop, _ := startClaudeStreamHost(t, cfg, directory)
	v, e := c.Open(t.Context(), host.Create, "legacy")
	if e != nil {
		t.Fatal(e)
	}
	stopMultiSession(t, c, v)
	stop()
	cfg.Providers = extra
	c2, _, _ := startClaudeStreamHost(t, cfg, directory)
	v, e = c2.Open(t.Context(), host.Resume, "legacy")
	if e != nil {
		t.Fatal("single-provider history could not resume", e)
	}
	// Signing out after boot must fail the selected Claude turn; Codex must
	// receive no speculative fallback request.
	f.Set(t, testclaude.Config{SignedOut: true, Subscription: "team", Catalog: hostClaudeCatalog})
	submitInteractive(t, c2, v, "signed-out", "safe prompt")
	failed := inspectInteractive(t, c2, "legacy", func(v host.View) bool { return !v.Running })
	if !strings.Contains(failed.Failure, "Claude Code is not signed in") || len(f.Calls(t)) != 0 || calls.Load() != 0 {
		t.Fatal("unavailable provider silently rerouted", failed.Failure)
	}
	// The next Host can boot with the unavailable provider registered.
	cfg2 := multiConfiguration(t, f, server.URL, true)
	c3, _, _ := startClaudeStreamHost(t, cfg2, privateCLIDirectory(t))
	v3, e := c3.Open(t.Context(), host.Create, "healthy-main")
	if e != nil {
		t.Fatal(e)
	}
	providers, e := modelExchange(t.Context(), c3.Extension, modelRequest{Action: "model.providers", ID: "healthy-main"})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, p := range providers.Providers {
		if p.ID == "claude-code" {
			found = true
			if p.Availability != "auth required" {
				t.Fatal("signin unavailable hidden", p.Availability)
			}
		}
	}
	if !found {
		t.Fatal("unavailable provider disappeared")
	}
	submitInteractive(t, c3, v3, "healthy", "hello")
	inspectInteractive(t, c3, "healthy-main", func(v host.View) bool { return responseCount(v) == 1 })
	if len(f.Calls(t)) != 0 {
		t.Fatal("unavailable backend invoked")
	}
}

func TestMultiProviderHealthIndependentOfConfiguredCatalogFallback(t *testing.T) {
	for _, unavailable := range []string{"binary missing", "unsupported version"} {
		t.Run(unavailable, func(t *testing.T) {
			f := testclaude.New(t)
			fake := testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog}
			if unavailable == "unsupported version" {
				fake.Version = "0.0.1"
			}
			f.Set(t, fake)
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("health/discovery sent a model request")
			}))
			t.Cleanup(server.Close)
			cfg := multiConfiguration(t, f, server.URL, true)
			if unavailable == "binary missing" {
				p := cfg.Providers["claude-code"]
				p.ClaudeCode.Binary = filepath.Join(f.Directory, "not-installed")
				cfg.Providers["claude-code"] = p
			}
			c, _, _ := startClaudeStreamHost(t, cfg, privateCLIDirectory(t))
			if _, e := c.Open(t.Context(), host.Create, "healthy-host"); e != nil {
				t.Fatal("an unavailable extra provider prevented Host startup", e)
			}
			out, e := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.providers", ID: "healthy-host"})
			if e != nil || len(out.Providers) != 2 {
				t.Fatal("provider health unavailable", e)
			}
			for _, p := range out.Providers {
				if p.ID == "claude-code" && (p.Availability != "unavailable" || !p.Catalog.Available) {
					t.Fatal("configured fallback concealed CLI unavailability")
				}
			}
			if len(f.Calls(t)) != 0 {
				t.Fatal("health/discovery sent Claude inference")
			}
		})
	}
}
