//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json/v2"
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
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
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
	started          bool
	// limit is the Host's lifetime; 4 minutes when zero.
	limit time.Duration
}

// startOpenCodeGoChildren is startGPTHost with OpenCode Go on a loopback
// server and its key stored; Codex is registered only with codex, on a server
// that counts the requests it should never get.
func startOpenCodeGoChildren(t *testing.T, codex, leadRuns bool) *openCodeGoChildren {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
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
	calls := withFakeCodex(t, &cfg, codex)
	h := &openCodeGoChildren{gptHost: &gptHost{binary: binary, gpt: gpt, claude: f}, server: server, codexCalls: calls, cfg: cfg}
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
	calls := &atomic.Int32{}
	t.Setenv("OPENAI_CODEX_AUTH_FILE", "")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "no-codex"))
	if !codex {
		return calls
	}
	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"Codex must not be used"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(codexServer.Close)
	auth := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(auth, 0700); err != nil {
		t.Fatal(err)
	}
	writeCodexAuth(t, filepath.Join(auth, "auth.json"), "gpt-token-sensitive", "gpt-account-sensitive")
	cache := `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"}]}]}`
	if err := os.WriteFile(filepath.Join(auth, "models_cache.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", auth)
	c := codexServeConfiguration(cfg.Runtime.Workspace, codexServer.URL).Runtime
	cfg.Providers["openai-codex"] = agentrunner.ProviderRuntime{Provider: c.Provider, ReasoningEffort: c.ReasoningEffort}
	cfg.Permissions.NetworkOrigins = append(cfg.Permissions.NetworkOrigins, codexServer.URL)
	return calls
}

// start starts a Host in a new directory: the same configuration, key store
// and /model-sub file, as after a restart.
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
	h.client, h.stop = startMultiChildCLIWith(t, h.binary, h.cfg, h.directory, limit, "--credential-directory", h.store, "--subagent-model-file", h.modelFile)
	h.started = true
}

func (h *openCodeGoChildren) cli(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), h.binary, append([]string{"subagent"}, args...)...)
	cmd.Env = append(os.Environ(), "UNREAL_AGENT_SOCKET="+filepath.Join(h.directory, "host.sock"), "UNREAL_AGENT_SESSION=designer")
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
		t.Fatal("/model-sub providers", out, err)
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
	}
	return report
}

var childHandle = regexp.MustCompile(`as (child-start-[0-9a-f]+)\.`)

// With Codex registered, choosing OpenCode Go with /model-sub runs the next
// lead and its workers on Flash at the template efforts in docs/opencode-go.md,
// without a restart and without a Codex request: the same run as
// TestSubagentCommandRunsLeadAndWorkers.
func TestSubagentCommandRunsLeadAndWorkersOnOpenCodeGo(t *testing.T) {
	h := startOpenCodeGoChildren(t, true, true)
	h.designer(t)
	listed := h.providers(t)
	if listed.SubagentModel.Provider != "openai-codex" || listed.Providers[0].Availability != "available" || listed.Providers[1].Availability != "available" || listed.Providers[1].Name != "OpenCode Go" {
		t.Fatal("before a choice", listed.Providers, listed.SubagentModel)
	}
	// A model OpenCode Go does not offer is refused, and nothing is saved.
	if _, err := h.choose(t, opencodego.ProviderID, "glm-5"); err == nil || !strings.Contains(err.Error(), "selection_unavailable") {
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

	started := h.mustCLI(t, "Implement the feature.\nVerify it.", "start", "--template", "lead")
	handle := childHandle.FindStringSubmatch(started)
	if handle == nil {
		t.Fatal(started)
	}
	waited := h.mustCLI(t, "", "wait", "--timeout", "60s")
	request := regexp.MustCompile(`approval needed: request (\S+): Bash \$ printf lead`).FindStringSubmatch(waited)
	if request == nil {
		t.Fatal("the lead's request did not reach the designer\n", waited, h.mustCLI(t, "", "list"))
	}
	if out := h.mustCLI(t, "", "approve", handle[1], request[1], "allow"); !strings.Contains(out, "Allowed request") {
		t.Fatal(out)
	}
	report := h.waitReport(t, handle[1], "lead done")
	if !strings.Contains(report, "result: completed") || !strings.Contains(report, "  - worker") {
		t.Fatal(report)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, handle[1]+"  lead (deepseek-v4.1-flash max)  completed  lead done") {
		t.Fatal(list)
	}
	if _, err := os.Stat(h.gpt.leadMarker); err != nil {
		t.Fatal("the lead's approved command did not run", err)
	}
	// The lead at max, its worker-high at high, the lead refused as a worker.
	h.check(t)
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

// With /model-sub = OpenCode Go, Claude is offered the children while
// OpenCode Go is registered and its key is saved, also while the public model
// list cannot be read; once the key is removed it does the work itself.
func TestClaudeIsOfferedOpenCodeGoChildrenWhileTheKeyIsSaved(t *testing.T) {
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
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, claudeagent.SubagentSocketEnv+"=") }) != children {
			t.Fatal("the subagent socket for this turn", children, env)
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

// While /model-sub chooses OpenCode Go, subagent start --model/--effort is
// refused before anything is recorded or sent; on Codex it starts as
// upstream's override does.
func TestSubagentOverridesAreRefusedWhileOpenCodeGoIsChosen(t *testing.T) {
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	if _, err := h.choose(t, opencodego.ProviderID, "deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--effort", "high"}, {"--model", "deepseek-v4-pro"}, {"--model", "gpt-6.1-sol", "--effort", "low"}} {
		out, err := h.cli(t, "Write the worker marker.", append([]string{"start", "--template", "worker-low"}, flags...)...)
		if err == nil || !strings.Contains(out, "override_unavailable: /model-sub で OpenCode Go を選んでいる間は --model と --effort を使えません") {
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
	if _, err := h.choose(t, "openai-codex", ""); err != nil {
		t.Fatal(err)
	}
	started := h.mustCLI(t, "Write the worker marker.", "start", "--template", "worker-low", "--effort", "high")
	if childHandle.FindStringSubmatch(started) == nil {
		t.Fatal(started)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "worker-low (gpt-6.1-sol high)") {
		t.Fatal("the override on Codex", list)
	}
}

// Without Codex there are no sub-agents until OpenCode Go is chosen; the
// choice says so, and the next Host runs a worker on Flash.
func TestOpenCodeGoSubagentsWithoutCodexAfterHostRestart(t *testing.T) {
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

// /model-sub offers Claude last on a Claude parent, and saves the chosen
// Claude model for Claude Code's own Agent sub-agents.
func TestClaudeIsOfferedAndSavedForClaudeCodeAgent(t *testing.T) {
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	listed := h.providers(t)
	if listed.Providers[2].Name != "Claude（サブスク）" || listed.Providers[2].Availability != "available" || listed.SubagentModel.Provider != "openai-codex" {
		t.Fatal("Claude in /model-sub", listed.Providers, listed.SubagentModel)
	}
	// A model Claude does not offer is refused, and nothing is saved.
	if _, err := h.choose(t, claudeagent.ProviderID, "not-a-model"); err == nil || !strings.Contains(err.Error(), "selection_unavailable") {
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

// While /model-sub chooses Claude, Claude works through Claude Code's own
// Agent tool: a built-in child template is refused with what to do instead,
// and nothing starts.
func TestClaudeChoiceRefusesBuiltInChildStarts(t *testing.T) {
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	if _, err := h.choose(t, claudeagent.ProviderID, "claude-configured-a"); err != nil {
		t.Fatal("choosing the Claude model", err)
	}
	out, err := h.cli(t, "Write the worker marker.", "start", "--template", "worker-low")
	if err == nil || !strings.Contains(out, "provider_unavailable: /model-sub で Claude を選んでいる間は子を起動できません。子を使うには /model-sub で OpenAI Codex か OpenCode Go を選んでください") {
		t.Fatalf("a child start on Claude: %v\n%s", err, out)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "No children in this Session.") {
		t.Fatal("a child started on Claude\n", list)
	}
	// Unreal's own override is refused too, saying what stays unavailable.
	if out, err := h.cli(t, "Write the worker marker.", "start", "--template", "worker-low", "--model", "claude-configured-a"); err == nil || !strings.Contains(out, "override_unavailable: /model-sub で Claude を選んでいる間は --model と --effort を使えません") {
		t.Fatalf("an override on Claude: %v\n%s", err, out)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, "No children in this Session.") {
		t.Fatal("a child started on Claude\n", list)
	}
}
