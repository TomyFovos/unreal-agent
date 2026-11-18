//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// openCodeGoServer is OpenCode Go on a loopback server: the public model list
// and Responses, answered by fakeGPT.
type openCodeGoServer struct {
	t    *testing.T
	gpt  *fakeGPT
	list []byte
	mu   sync.Mutex
	// What the requests carried.
	keys, models          []string
	sessions              int
	leadRule, codexModels bool
	// listFails makes the model list unreadable.
	listFails atomic.Bool
}

func (s *openCodeGoServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/inference/go/v1/models" {
		if r.Header.Get("Authorization") != "" {
			s.t.Error("the model list request carried a key")
		}
		if s.listFails.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.list)
		return
	}
	if r.URL.Path != "/inference/go/openai/v1/responses" {
		s.t.Error("request outside OpenCode Go's Responses", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Error(err)
		return
	}
	var request struct {
		Model string `json:"model"`
	}
	if err = json.Unmarshal(body, &request); err != nil {
		s.t.Error(err)
		return
	}
	s.mu.Lock()
	s.keys = append(s.keys, r.Header.Get("Authorization"))
	s.models = append(s.models, request.Model)
	if r.Header.Get("x-opencode-session") != "" {
		s.sessions++
	}
	if bytes.Contains(body, []byte("You are the implementation lead")) {
		s.leadRule = s.leadRule || bytes.Contains(body, []byte("collector template (deepseek-v4.1-flash at low effort), never to a higher-effort worker"))
		s.codexModels = s.codexModels || bytes.Contains(body, []byte("gpt-6"))
	}
	s.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.gpt.ServeHTTP(w, r)
}

// check: every model request went to Flash with the stored key.
func (s *openCodeGoServer) check(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.models) == 0 || s.sessions != len(s.models) {
		t.Fatal("OpenCode Go requests", len(s.models), s.sessions)
	}
	for i := range s.models {
		if s.models[i] != "deepseek-v4.1-flash" || s.keys[i] != "Bearer opencode-go-key-sensitive" {
			t.Fatal("request", i, s.models[i])
		}
	}
}

type openCodeGoChildren struct {
	*gptHost
	server           *openCodeGoServer
	codexCalls       *atomic.Int32
	cfg              serveConfiguration
	store, modelFile string
	codexAuth        string
	started          bool
	// limit is the Host's lifetime; 4 minutes when zero.
	limit time.Duration
}

// startOpenCodeGoChildren is startGPTHost with OpenCode Go on a loopback
// server and its key stored; Codex is registered only with codex, on a server
// that counts the requests it should never get.
func startOpenCodeGoChildren(t *testing.T, codex, leadRuns bool, codexResponds ...bool) *openCodeGoChildren {
	t.Helper()
	binary := testCLI(t, true)
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "noted"})
	cfg := claudeRouteConfiguration(t, f)
	workspace := cfg.Runtime.Workspace
	gpt := &fakeGPT{t: t, leadMarker: filepath.Join(workspace, "lead"), workerMarker: filepath.Join(workspace, "worker"), leadRuns: leadRuns}
	list, err := os.ReadFile(openCodeGoTestList(t))
	if err != nil {
		t.Fatal(err)
	}
	server := &openCodeGoServer{t: t, gpt: gpt, list: list}
	endpoint := httptest.NewServer(server)
	t.Cleanup(endpoint.Close)
	route := openCodeGoRoute("deepseek-v4.1-flash")
	route.Endpoint = endpoint.URL + "/inference/go/openai/v1"
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: route, ReasoningEffort: "high"}}
	cfg.Permissions.NetworkOrigins = []string{endpoint.URL}
	var responders []http.Handler
	if len(codexResponds) > 0 && codexResponds[0] {
		responders = append(responders, gpt)
	}
	calls, auth := fakeCodexSource(t, &cfg, codex, responders...)
	h := &openCodeGoChildren{gptHost: &gptHost{binary: binary, gpt: gpt, claude: f, environment: testCLIEnvironment(t)}, server: server, codexCalls: calls, cfg: cfg, codexAuth: auth}
	h.store = filepath.Join(privateCLIDirectory(t), "credentials")
	storeKey(t, h.store, openCodeGoKey, "opencode-go-key-sensitive")
	h.modelFile = filepath.Join(privateCLIDirectory(t), "subagent-model.json")
	h.start(t)
	return h
}

// withFakeCodex keeps the Mac's Codex out of the Host and, with codex,
// registers Codex on a server that counts the requests it should never get.
func withFakeCodex(t *testing.T, cfg *serveConfiguration, codex bool) *atomic.Int32 {
	t.Helper()
	calls, auth := fakeCodexSource(t, cfg, codex)
	t.Setenv("OPENAI_CODEX_AUTH_FILE", "")
	t.Setenv("CODEX_HOME", filepath.Dir(auth))
	return calls
}

func fakeCodexSource(t *testing.T, cfg *serveConfiguration, codex bool, responders ...http.Handler) (*atomic.Int32, string) {
	t.Helper()
	calls := &atomic.Int32{}
	if !codex {
		return calls, filepath.Join(t.TempDir(), "no-codex", "auth.json")
	}
	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if len(responders) > 0 {
			responders[0].ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"error":{"message":"Codex must not be used"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(codexServer.Close)
	auth := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(auth, 0700); err != nil {
		t.Fatal(err)
	}
	writeCodexAuth(t, filepath.Join(auth, "auth.json"), "gpt-token-sensitive", "gpt-account-sensitive")
	cache := `{"models":[{"slug":"gpt-6.2-sol","display_name":"GPT-6.2 Sol","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"}]}]}`
	if err := os.WriteFile(filepath.Join(auth, "models_cache.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	c := codexServeConfiguration(cfg.Runtime.Workspace, codexServer.URL).Runtime
	cfg.Providers["openai-codex"] = agentrunner.ProviderRuntime{Provider: c.Provider, ReasoningEffort: c.ReasoningEffort}
	cfg.Permissions.NetworkOrigins = append(cfg.Permissions.NetworkOrigins, codexServer.URL)
	return calls, filepath.Join(auth, "auth.json")
}

func (h *openCodeGoChildren) chooseRole(t *testing.T, role, provider, model string, effort llm.ReasoningEffort) (modelReply, error) {
	t.Helper()
	args := []string{"model", "set", "--role", role, "--provider", provider, "--model", model}
	if effort != "" {
		args = append(args, "--effort", string(effort))
	}
	output, err := h.cli(t, "", args...)
	if err != nil {
		return modelReply{}, fmt.Errorf("%s: %w", strings.TrimSpace(output), err)
	}
	return modelReply{Pending: strings.Contains(output, "restart the Host")}, nil
}

func TestSubagentRoleSelectionRoutingAndRestart(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false, true)
	h.gpt.mu.Lock()
	h.gpt.routingOnly = true
	h.gpt.mu.Unlock()
	h.designer(t)
	for _, c := range []struct {
		role, provider, model string
		effort                llm.ReasoningEffort
	}{
		{"lead", "openai-codex", leadModel, ""},
		{"worker", opencodego.ProviderID, "deepseek-v4.1-flash", "max"},
		{"collector", opencodego.ProviderID, "kimi-k2.7-code", ""},
	} {
		if out, err := h.chooseRole(t, c.role, c.provider, c.model, c.effort); err != nil || out.Pending {
			t.Fatalf("%s: %+v, %v", c.role, out, err)
		}
	}
	for _, name := range []string{"lead", "worker-low", "worker-medium", "worker-high", "worker-xhigh", "worker-max", "collector"} {
		started := h.mustCLI(t, "Check selected routing.", "start", "--template", name)
		handle := childHandle.FindStringSubmatch(started)
		if handle == nil {
			t.Fatal(started)
		}
		summary := "worker done"
		if name == "lead" {
			summary = "lead done"
		} else if name == "collector" {
			summary = "collector done"
		}
		h.waitReport(t, handle[1], summary)
	}
	// Overriding a Codex lead must also keep its Go worker bindings.
	override := h.mustCLI(t, "Keep the workers' separate provider.", "start", "--template", "lead", "--effort", "max")
	h.waitReport(t, childHandle.FindStringSubmatch(override)[1], "lead done")
	h.gpt.mu.Lock()
	leads, workers := slices.Clone(h.gpt.leadEfforts), slices.Clone(h.gpt.workerEffort)
	leadModels := slices.Clone(h.gpt.leadModels)
	h.gpt.mu.Unlock()
	if len(leads) == 0 || slices.ContainsFunc(leads, func(e string) bool { return e != "max" }) || len(workers) < len(workerEfforts)+1 || slices.ContainsFunc(workers, func(e string) bool { return e != "max" }) {
		t.Fatalf("wire efforts: lead=%v worker=%v", leads, workers)
	}
	if len(leadModels) == 0 || slices.ContainsFunc(leadModels, func(model string) bool { return model != "gpt-6.2-sol" }) {
		t.Fatalf("Codex lead wire models: %v", leadModels)
	}
	if h.codexCalls.Load() == 0 {
		t.Fatal("the lead did not use the Codex fake")
	}
	h.server.mu.Lock()
	models := slices.Clone(h.server.models)
	h.server.mu.Unlock()
	if !slices.Contains(models, "deepseek-v4.1-flash") || !slices.Contains(models, "kimi-k2.7-code") || slices.Contains(models, leadModel) {
		t.Fatalf("Go wire models: %v", models)
	}
	before := h.mustCLI(t, "", "model", "show")
	h.start(t)
	h.designer(t)
	after := h.mustCLI(t, "", "model", "show")
	if before != after {
		t.Fatalf("restart changed choices: %s -> %s", before, after)
	}
	started := h.mustCLI(t, "Use restored worker.", "start", "--template", "worker-low")
	h.waitReport(t, childHandle.FindStringSubmatch(started)[1], "worker done")
	// U33 also permits the inverse: Go owns the issue, Codex does the work.
	if _, err := h.chooseRole(t, "lead", opencodego.ProviderID, "deepseek-v4.1-flash", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.chooseRole(t, "worker", "openai-codex", leadModel, "high"); err != nil {
		t.Fatal(err)
	}
	started = h.mustCLI(t, "Verify the inverse role assignment.", "start", "--template", "lead")
	h.waitReport(t, childHandle.FindStringSubmatch(started)[1], "lead done")
	h.gpt.mu.Lock()
	lastWorker := h.gpt.workerEffort[len(h.gpt.workerEffort)-1]
	h.gpt.mu.Unlock()
	if lastWorker != "high" {
		t.Fatalf("Codex worker ignored its group's effort: %s", lastWorker)
	}
}

func TestSubagentUnavailableWorkerDoesNotFallback(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false, true)
	h.designer(t)
	if _, err := h.chooseRole(t, "worker", opencodego.ProviderID, "deepseek-v4.1-flash", "max"); err != nil {
		t.Fatal(err)
	}
	store, err := credential.OpenLocal(h.store)
	if err != nil {
		t.Fatal(err)
	}
	if err = authflow.New(credential.NewManager(store)).Logout(t.Context(), openCodeGoKey); err != nil {
		t.Fatal(err)
	}
	if out, err := h.cli(t, "", "model", "set", "--role", "worker", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "max"); err == nil || !strings.Contains(out, "provider_unavailable") {
		t.Fatalf("unavailable provider accepted by model set: %s, %v", out, err)
	}
	out, err := h.cli(t, "Must fail on the chosen provider.", "start", "--template", "worker-low")
	if err == nil || !strings.Contains(out, "provider_unavailable") || !strings.Contains(out, "/login") {
		t.Fatalf("%s, %v", out, err)
	}
	if h.codexCalls.Load() != 0 {
		t.Fatal("worker fell back to Codex")
	}
	h.server.mu.Lock()
	defer h.server.mu.Unlock()
	if len(h.server.models) != 0 {
		t.Fatal("a worker request was sent without its key")
	}
}

func TestSubagentRoleClaudeGuidance(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	view := h.designer(t)
	if _, err := h.chooseRole(t, "lead", "openai-codex", leadModel, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.chooseRole(t, "worker", opencodego.ProviderID, "deepseek-v4.1-flash", "max"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.chooseRole(t, "collector", claudeagent.ProviderID, "claude-configured-a", ""); err != nil {
		t.Fatal(err)
	}
	turn := func(id string, count int) string {
		t.Helper()
		submitInteractive(t, h.client, view, id, "hello")
		inspectInteractive(t, h.client, "designer", func(v host.View) bool { return responseCount(v) >= count && v.Progress == nil })
		calls := h.claude.Calls(t)
		call := calls[len(calls)-1]
		if slices.ContainsFunc(call.Environment, func(s string) bool { return strings.HasPrefix(s, "CLAUDE_CODE_SUBAGENT_MODEL") }) {
			t.Fatal("a mixed selection forced every Agent to the collector's model")
		}
		return strings.Join(call.Arguments, "\n")
	}
	prompt := turn("role-guidance", 1)
	for _, text := range []string{
		"subagent model show",
		"subagent model set --role lead|worker|collector|all",
		"lead: openai-codex · gpt-6.2-sol · max effort · Templates: lead",
		"worker: opencode-go · deepseek-v4.1-flash · max effort · Templates: worker-high, worker-low, worker-max, worker-medium, worker-xhigh",
		`use Claude Code's Agent tool with model set to "claude-configured-a" for this role`,
	} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("missing %q in guidance:\n%s", text, prompt)
		}
	}
	if strings.Contains(prompt, "--template collector") {
		t.Fatal("a Claude collector was advertised as an Unreal child")
	}
	store, err := credential.OpenLocal(h.store)
	if err != nil {
		t.Fatal(err)
	}
	if err = authflow.New(credential.NewManager(store)).Logout(t.Context(), openCodeGoKey); err != nil {
		t.Fatal(err)
	}
	prompt = turn("unavailable-worker-guidance", 2)
	if !strings.Contains(prompt, "lead: openai-codex · gpt-6.2-sol · max effort · Templates: lead") || !strings.Contains(prompt, "worker: opencode-go · deepseek-v4.1-flash · max effort · unavailable") || strings.Contains(prompt, "--template worker-") {
		t.Fatalf("availability did not stay per role:\n%s", prompt)
	}
}

// start starts a Host in a new directory: the same configuration, key store
// and subagent model file, as after a restart.
func (h *openCodeGoChildren) start(t *testing.T) {
	t.Helper()
	if h.started {
		h.stop()
	}
	limit := h.limit
	if limit == 0 {
		limit = 4 * time.Minute
	}
	h.directory = privateCLIDirectory(t)
	h.client, h.stop = startMultiChildCLIEnvironment(t, h.binary, h.cfg, h.directory, limit, h.environment, "--credential-directory", h.store, "--subagent-model-file", h.modelFile, "--codex-auth-file", h.codexAuth)
	h.started = true
}

func (h *openCodeGoChildren) cli(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), h.binary, append([]string{"subagent"}, args...)...)
	cmd.Env = append(slices.Clone(h.environment), "UNREAL_AGENT_SOCKET="+filepath.Join(h.directory, "host.sock"), "UNREAL_AGENT_SESSION=designer")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *openCodeGoChildren) mustCLI(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := h.cli(t, stdin, args...)
	if err != nil {
		t.Fatalf("subagent %v: %v\n%s", args, err, out)
	}
	return out
}

func (h *openCodeGoChildren) providers(t *testing.T) modelReply {
	t.Helper()
	out, err := modelExchange(t.Context(), h.client.Extension, modelRequest{Action: "subagent.providers", ID: "designer"})
	if err != nil || len(out.Providers) != 3 || out.Providers[0].ID != "openai-codex" || out.Providers[1].ID != opencodego.ProviderID || out.Providers[2].ID != claudeagent.ProviderID || out.SubagentModel == nil {
		t.Fatal("subagent model providers", out, err)
	}
	return out
}

func (h *openCodeGoChildren) choose(t *testing.T, provider, model string) (modelReply, error) {
	t.Helper()
	return modelExchange(t.Context(), h.client.Extension, modelRequest{Action: "subagent.select", ID: "designer", Selection: sessionstore.RuntimeSelection{Provider: provider, Model: model}})
}

// waitReport waits until the child reports summary.
func (h *openCodeGoChildren) waitReport(t *testing.T, handle, summary string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	report := ""
	for !strings.Contains(report, "summary: "+summary) {
		if time.Now().After(deadline) {
			t.Fatal("the child did not finish\n", report)
		}
		report = h.mustCLI(t, "", "wait", handle, "--timeout", "30s")
		if strings.Contains(report, "  failed\n") || strings.Contains(report, "  canceled\n") {
			t.Fatal("the child ended without its expected result\n", report)
		}
	}
	return report
}

var childHandle = regexp.MustCompile(`as (child-start-[0-9a-f]+)\.`)

// With Codex registered, choosing OpenCode Go with subagent model runs the next
// lead and its worker on Flash without restart or a Codex request. CLI approval,
// shell execution and worker authority are covered by the provider-independent
// TestSubagentCommandRunsLeadAndWorkers; this run checks the binding and wire.
func TestOpenCodeGoSubagentSelectionAndRuntimeRouting(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	h.gpt.mu.Lock()
	h.gpt.routingOnly = true
	h.gpt.mu.Unlock()
	view := h.designer(t)
	listed := h.providers(t)
	if listed.SubagentModel.Provider != "openai-codex" || listed.Providers[0].Availability != "available" || listed.Providers[1].Availability != "available" || listed.Providers[1].Name != "OpenCode Go" {
		t.Fatal("before a choice", listed.Providers, listed.SubagentModel)
	}
	// A model OpenCode Go does not offer is refused, and nothing is saved.
	if _, err := h.choose(t, opencodego.ProviderID, "glm-5"); err == nil || !strings.Contains(err.Error(), "invalid_model") {
		t.Fatal("an unoffered model was accepted", err)
	}
	if _, err := os.Lstat(h.modelFile); !os.IsNotExist(err) {
		t.Fatal("a refused choice was saved", err)
	}
	chosen, err := h.choose(t, opencodego.ProviderID, "deepseek-v4.1-flash")
	if err != nil || chosen.SubagentModel == nil || chosen.SubagentModel.Model != "deepseek-v4.1-flash" || chosen.SubagentModel.Name != "DeepSeek V4.1 Flash" || chosen.Pending {
		t.Fatal("choosing Flash", chosen, err)
	}
	if m := h.providers(t).SubagentModel; m.Provider != opencodego.ProviderID || m.Model != "deepseek-v4.1-flash" {
		t.Fatal("the choice was not kept", m)
	}

	input := inbox.ID("routing-lead")
	if _, err := modelExchange(t.Context(), h.client.Extension, modelRequest{Action: "child.start", ID: "designer", Generation: view.Generation, InputID: input, Template: "lead", Task: "Implement the feature.\nVerify it."}); err != nil {
		t.Fatal(err)
	}
	handle := subagent.StartOperationID("designer", input)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var completed operation.Operation
	end, err := waitSubscribedView(ctx, h.client, "designer", func(v host.View) bool {
		for _, op := range v.Operations {
			if op.ID == handle && (op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled) {
				completed = op
				return true
			}
		}
		return false
	})
	if err != nil || completed.Status != operation.StatusCompleted || len(end.Permissions.Pending) != 0 {
		t.Fatal("selected lead did not complete without a human request", err, completed.Status)
	}
	result, err := subagent.DecodeHandle(completed)
	if err != nil || result.Finish == nil || result.Finish.Result.Status != "completed" || result.Finish.Result.Summary != "lead done" || !slices.Contains(result.Finish.Result.ChangedFiles, "worker") {
		t.Fatal("selected lead's result", result, err)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, string(handle)+"  lead (deepseek-v4.1-flash max)  completed  lead done") {
		t.Fatal(list)
	}
	// Both real model transports must carry the chosen model and effort,
	// including the worker launched by this lead, not just a top-level worker.
	h.gpt.mu.Lock()
	leadEfforts, workerEfforts := slices.Clone(h.gpt.leadEfforts), slices.Clone(h.gpt.workerEffort)
	h.gpt.mu.Unlock()
	if len(leadEfforts) == 0 || slices.ContainsFunc(leadEfforts, func(e string) bool { return e != "max" }) || len(workerEfforts) == 0 || slices.ContainsFunc(workerEfforts, func(e string) bool { return e != "high" }) {
		t.Fatal("selected lead/worker efforts", leadEfforts, workerEfforts)
	}
	h.server.check(t)
	h.server.mu.Lock()
	leadRule, codexModels := h.server.leadRule, h.server.codexModels
	h.server.mu.Unlock()
	if !leadRule || codexModels {
		t.Fatal("the lead's prompt does not name Flash", leadRule, codexModels)
	}
	if n := h.codexCalls.Load(); n != 0 {
		t.Fatal("Codex was used", n)
	}
}

// With subagent model = OpenCode Go, Claude is offered the children while
// OpenCode Go is registered and its key is saved, also while the public model
// list cannot be read; once the key is removed it does the work itself.
func TestClaudeIsOfferedOpenCodeGoChildrenWhileTheKeyIsSaved(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	view := h.designer(t)
	if _, err := h.choose(t, opencodego.ProviderID, "deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	turn := func(id string, n int, children bool) {
		t.Helper()
		submitInteractive(t, h.client, view, id, "hello")
		inspectInteractive(t, h.client, "designer", func(v host.View) bool { return responseCount(v) >= n && v.Progress == nil })
		assertClaudeDelegation(t, h.claude, children)
		calls := h.claude.Calls(t)
		env := calls[len(calls)-1].Environment
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, claudeagent.SubagentSocketEnv+"=") }) != true {
			t.Fatal("the settings CLI lost its Host socket", env)
		}
	}
	turn("a", 1, true)
	h.server.listFails.Store(true)
	if out, err := modelExchange(t.Context(), h.client.Extension, modelRequest{Action: "model.catalog", ID: "designer", Provider: opencodego.ProviderID, Refresh: true}); err != nil || out.Catalog == nil || out.Catalog.Available {
		t.Fatal("the list read did not fail", out, err)
	}
	turn("b", 2, true)
	store, err := credential.OpenLocal(h.store)
	if err != nil {
		t.Fatal(err)
	}
	if err = authflow.New(credential.NewManager(store)).Logout(t.Context(), openCodeGoKey); err != nil {
		t.Fatal(err)
	}
	turn("c", 3, false)
	// A start without the key says once what to do.
	out, err := h.cli(t, "Write the worker marker.", "start", "--template", "worker-low")
	if err == nil || !strings.Contains(out, "provider_unavailable: /login の手順で API キーを保存し、/model refresh を実行してください") || strings.Contains(out, "ログイン後") {
		t.Fatal("the start without the key", err, out)
	}
	if n := h.codexCalls.Load(); n != 0 {
		t.Fatal("Codex was used", n)
	}
}

// While subagent model chooses OpenCode Go, subagent start --model/--effort is
// refused before anything is recorded or sent; on Codex it starts as
// upstream's override does.
func TestSubagentOverridesAreRefusedWhileOpenCodeGoIsChosen(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	if _, err := h.choose(t, opencodego.ProviderID, "deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--effort", "high"}, {"--model", "deepseek-v4-pro"}, {"--model", "gpt-6.2-sol", "--effort", "low"}} {
		out, err := h.cli(t, "Write the worker marker.", append([]string{"start", "--template", "worker-low"}, flags...)...)
		if err == nil || !strings.Contains(out, "override_unavailable: subagent model で OpenCode Go を選んでいる間は --model と --effort を使えません") {
			t.Fatal("an override started on OpenCode Go", flags, err, out)
		}
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "No children in this Session.") {
		t.Fatal("a refused start was recorded", list)
	}
	h.server.mu.Lock()
	requests := len(h.server.models)
	h.server.mu.Unlock()
	if n := h.codexCalls.Load(); n != 0 || requests != 0 {
		t.Fatal("a refused start sent requests", n, requests)
	}
	if _, err := h.choose(t, "openai-codex", leadModel); err != nil {
		t.Fatal(err)
	}
	started := h.mustCLI(t, "Write the worker marker.", "start", "--template", "worker-low", "--effort", "high")
	if childHandle.FindStringSubmatch(started) == nil {
		t.Fatal(started)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "worker-low (gpt-6.2-sol high)") {
		t.Fatal("the override on Codex", list)
	}
}

// Without Codex there are no sub-agents until OpenCode Go is chosen; the
// choice says so, and the next Host runs a worker on Flash.
func TestOpenCodeGoSubagentsWithoutCodexAfterHostRestart(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, false, false)
	h.designer(t)
	listed := h.providers(t)
	if listed.Providers[0].Availability == "available" || listed.Providers[1].Availability != "available" {
		t.Fatal("providers without Codex", listed.Providers)
	}
	if out, err := h.cli(t, "printf worker", "start", "--template", "worker-low"); err == nil {
		t.Fatal("a worker started without sub-agents", out)
	}
	chosen, err := h.choose(t, opencodego.ProviderID, "deepseek-v4.1-flash")
	if err != nil || !chosen.Pending {
		t.Fatal("the choice did not say it waits for the next Host", chosen, err)
	}

	h.start(t)
	h.designer(t)
	started := h.mustCLI(t, "Write the worker marker.", "start", "--template", "worker-low")
	handle := childHandle.FindStringSubmatch(started)
	if handle == nil {
		t.Fatal(started)
	}
	waited := h.mustCLI(t, "", "wait", "--timeout", "60s")
	request := regexp.MustCompile(`approval needed: request (\S+): Bash \$ printf worker`).FindStringSubmatch(waited)
	if request == nil {
		t.Fatal("the worker's request did not reach the designer\n", waited)
	}
	h.mustCLI(t, "", "approve", handle[1], request[1], "allow")
	h.waitReport(t, handle[1], "worker done")
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, handle[1]+"  worker-low (deepseek-v4.1-flash low)  completed  worker done") {
		t.Fatal(list)
	}
	h.gpt.mu.Lock()
	efforts := slices.Clone(h.gpt.workerEffort)
	h.gpt.mu.Unlock()
	if len(efforts) == 0 || slices.ContainsFunc(efforts, func(e string) bool { return e != "low" }) {
		t.Fatal("worker efforts", efforts)
	}
	h.server.check(t)
	if _, err := os.Stat(h.gpt.workerMarker); err != nil {
		t.Fatal("the worker's command did not run", err)
	}
}

// subagent model offers Claude last on a Claude parent, and saves the chosen
// Claude model for Claude Code's own Agent sub-agents.
func TestClaudeIsOfferedAndSavedForClaudeCodeAgent(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	listed := h.providers(t)
	if listed.Providers[2].Name != "Claude（サブスク）" || listed.Providers[2].Availability != "available" || listed.SubagentModel.Provider != "openai-codex" {
		t.Fatal("Claude in subagent model", listed.Providers, listed.SubagentModel)
	}
	// A model Claude does not offer is refused, and nothing is saved.
	if _, err := h.choose(t, claudeagent.ProviderID, "not-a-model"); err == nil || !strings.Contains(err.Error(), "invalid_model") {
		t.Fatal("an unknown Claude model was accepted", err)
	}
	if _, err := os.Stat(h.modelFile); !os.IsNotExist(err) {
		t.Fatal("a refused choice was saved", err)
	}
	chosen, err := h.choose(t, claudeagent.ProviderID, "claude-configured-a")
	if err != nil || chosen.SubagentModel == nil || chosen.SubagentModel.Provider != claudeagent.ProviderID || chosen.SubagentModel.Model != "claude-configured-a" || chosen.SubagentModel.Name != "Discovered A" {
		t.Fatal("choosing the Claude model", chosen, err)
	}
	saved, err := os.ReadFile(h.modelFile)
	if err != nil || string(saved) != `{"Version":1,"Provider":"claude","Model":"claude-configured-a"}` {
		t.Fatal("the saved choice", string(saved), err)
	}
	if listed := h.providers(t); listed.SubagentModel.Provider != claudeagent.ProviderID || listed.SubagentModel.Model != "claude-configured-a" {
		t.Fatal("the Claude choice is not read back", listed.SubagentModel)
	}
}

// While subagent model chooses Claude, Claude works through Claude Code's own
// Agent tool: a built-in child template is refused with what to do instead,
// and nothing starts.
func TestClaudeChoiceRefusesBuiltInChildStarts(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	if _, err := h.choose(t, claudeagent.ProviderID, "claude-configured-a"); err != nil {
		t.Fatal("choosing the Claude model", err)
	}
	out, err := h.cli(t, "Write the worker marker.", "start", "--template", "worker-low")
	if err == nil || !strings.Contains(out, "provider_unavailable: subagent model で Claude を選んでいる間は子を起動できません。子を使うには subagent model で OpenAI Codex か OpenCode Go を選んでください") {
		t.Fatalf("a child start on Claude: %v\n%s", err, out)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "No children in this Session.") {
		t.Fatal("a child started on Claude\n", list)
	}
	// Unreal's own override is refused too, saying what stays unavailable.
	if out, err := h.cli(t, "Write the worker marker.", "start", "--template", "worker-low", "--model", "claude-configured-a"); err == nil || !strings.Contains(out, "override_unavailable: subagent model で Claude を選んでいる間は --model と --effort を使えません") {
		t.Fatalf("an override on Claude: %v\n%s", err, out)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "No children in this Session.") {
		t.Fatal("a child started on Claude\n", list)
	}
}
