//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// liveModel is the model the Host live checks run: UNREAL_OPENCODE_GO_MODEL,
// or the first offered model (Flash).
func liveModel() string {
	if value := strings.TrimSpace(os.Getenv("UNREAL_OPENCODE_GO_MODEL")); value != "" {
		return value
	}
	return opencodego.OfferedModels()[0]
}

// The live Host check starts a normal Host on the user's stored key, reads
// the real model list, switches the Session to OpenCode Go and sends one turn
// (docs/opencode-go.md, テスト方針). It runs only with UNREAL_LIVE_OPENCODE_GO=1,
// on UNREAL_OPENCODE_GO_MODEL (default Flash). The test never reads the key;
// the Host does, for each request.
func TestLiveOpenCodeGoThroughHost(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_OPENCODE_GO") != "1" {
		t.Skip("set UNREAL_LIVE_OPENCODE_GO=1 to send one turn through a Host with the stored key")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(home, ".local", "state", "unreal-agent", "credentials")
	f, cli := normalFixture(t, true, true)
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.catalog", ID: id, Provider: opencodego.ProviderID, Refresh: true})
		if err != nil || out.Catalog == nil || !out.Catalog.Available {
			t.Fatal("real model list", err)
		}
		model, e := out.Catalog.FindSelection(liveModel())
		if e != nil {
			t.Fatalf("the model list has no %s: %s", liveModel(), out.Catalog.ProblemCode)
		}
		t.Logf("model list: %d models; checking %s %q efforts %v default %s context %d", len(out.Catalog.Models), model.ID, model.Name, model.Efforts, model.DefaultEffort, model.ContextWindow)
		v, err := c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("permission mode: %s", v.Permissions.Mode)
		stopMultiSession(t, c, v)
		v, err = c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "live-opencode-go", Provider: opencodego.ProviderID, Model: model.ID, Effort: model.DefaultEffort}
		if _, err = modelExchange(ctx, c.Extension, modelRequest{Action: "model.select", ID: id, Generation: v.Generation, Selection: choice}); err != nil {
			t.Fatal("switch to OpenCode Go", err)
		}
		resumed, err := c.Open(ctx, host.Resume, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Submit(ctx, id, resumed.Generation, inbox.Input{ID: "live-turn", Kind: inbox.InputExternal, Payload: []byte(`"Reply with the single word: ok"`)}); err != nil {
			t.Fatal(err)
		}
		end, text := liveAwait(t, c, id, 3*time.Minute)
		if end.Failure != "" {
			t.Fatal("turn failed:", end.Failure)
		}
		if selection := multiReport(t, end).Selection; selection == nil || selection.Provider != opencodego.ProviderID {
			t.Fatal("the turn did not run on OpenCode Go")
		}
		var usage llm.Usage
		for _, item := range end.History.Items {
			if r, ok := item.Data.(sessionstore.ModelResponse); ok {
				usage = r.Response.Usage
			}
		}
		t.Logf("reply %q, input %d, cached %d, output %d tokens", text, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens)
		if !strings.Contains(strings.ToLower(text), "ok") {
			t.Error("unexpected reply", text)
		}
		if f.calls.Load() != 0 || len(cli.Calls(t)) != 0 {
			t.Fatal("the turn went to Codex or Claude")
		}
		return nil
	}}
	if err := l.run(t.Context(), []string{"--credential-directory", store}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// liveSubagentBudget is the most Responses requests the live sub-agent check
// sends to OpenCode Go (approved by the user on 2026-11-04).
const liveSubagentBudget = 20

// liveOpenCodeGoProxy forwards OpenCode Go's model list and Responses from a
// loopback endpoint to opencode.ai, so the test sees each request the Host
// sends. It refuses the Responses past the budget without sending them. It
// passes the key the Host sends on and never records it.
type liveOpenCodeGoProxy struct {
	t       *testing.T
	forward *httputil.ReverseProxy
	mu      sync.Mutex
	// What the Responses requests carried, refused ones included.
	requests []liveRequest
	sent     int
	// wantRule is the collector line the lead's prompt must carry.
	wantRule              string
	leadRule, codexModels bool
}

type liveRequest struct {
	role, model, effort string
	status              int
	input, cached       int
	refused, key        bool
}

func newLiveOpenCodeGoProxy(t *testing.T) *liveOpenCodeGoProxy {
	target, err := url.Parse("https://opencode.ai")
	if err != nil {
		t.Fatal(err)
	}
	return &liveOpenCodeGoProxy{t: t, forward: &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}}
}

func (p *liveOpenCodeGoProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/inference/go/v1/models":
		if r.Header.Get("Authorization") != "" {
			p.t.Error("the model list request carried a key")
			http.Error(w, "the model list takes no key", http.StatusBadRequest)
			return
		}
		p.forward.ServeHTTP(w, r)
		return
	case "/inference/go/openai/v1/responses":
	default:
		p.t.Error("request outside OpenCode Go's model list and Responses", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Error(err)
		return
	}
	var request struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err = json.Unmarshal(body, &request); err != nil {
		p.t.Error(err)
	}
	role := "other"
	switch {
	case bytes.Contains(body, []byte("You are the implementation lead")):
		role = "lead"
	case bytes.Contains(body, []byte("You are a worker")):
		role = "worker"
	case bytes.Contains(body, []byte("You are a collector")):
		role = "collector"
	}
	p.mu.Lock()
	i := len(p.requests)
	p.requests = append(p.requests, liveRequest{role: role, model: request.Model, effort: request.Reasoning.Effort, key: strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")})
	if p.sent >= liveSubagentBudget {
		p.requests[i].refused = true
		p.mu.Unlock()
		http.Error(w, `{"error":{"message":"the live check's request budget is used up"}}`, http.StatusBadRequest)
		return
	}
	p.sent++
	if role == "lead" {
		p.leadRule = p.leadRule || p.wantRule != "" && bytes.Contains(body, []byte(p.wantRule))
		p.codexModels = p.codexModels || bytes.Contains(body, []byte("gpt-6"))
	}
	p.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	status := &liveStatus{ResponseWriter: w}
	p.forward.ServeHTTP(status, r)
	input, cached := status.usage()
	p.mu.Lock()
	p.requests[i].status, p.requests[i].input, p.requests[i].cached = status.code, input, cached
	p.mu.Unlock()
}

// report logs each Responses request and returns them.
func (p *liveOpenCodeGoProxy) report(t *testing.T) []liveRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.requests {
		t.Logf("request %d: %s %s %s status %d input %d cached %d refused %t", i+1, r.role, r.model, r.effort, r.status, r.input, r.cached, r.refused)
	}
	t.Logf("sent %d of %d Responses requests to OpenCode Go", p.sent, len(p.requests))
	return append([]liveRequest(nil), p.requests...)
}

func (p *liveOpenCodeGoProxy) overBudget() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests) > p.sent
}

// liveStatus keeps the status OpenCode Go answered with and the event stream,
// from which usage reads the token counts; Unwrap lets the proxy flush the
// stream.
type liveStatus struct {
	http.ResponseWriter
	code int
	body bytes.Buffer
}

// usage reads the input and cached token counts from the streamed
// response.completed event. Logging only: the check asserts nothing about them.
func (s *liveStatus) usage() (input, cached int) {
	for _, line := range strings.Split(s.body.String(), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Usage struct {
					InputTokens        int `json:"input_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &event) != nil || event.Type != "response.completed" {
			continue
		}
		input, cached = event.Response.Usage.InputTokens, event.Response.Usage.InputTokensDetails.CachedTokens
	}
	return input, cached
}

func (s *liveStatus) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *liveStatus) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	s.body.Write(b)
	return s.ResponseWriter.Write(b)
}

func (s *liveStatus) Unwrap() http.ResponseWriter { return s.ResponseWriter }

const liveLeadTask = `Live check: one worker reads one file.
Start exactly one worker with the worker-low template and this task: "Read the file live-word.txt in the workspace with the read tool and put the single word it contains in your Finish summary. Run no commands and change no files."
Do nothing else yourself: run no commands, read no files and start no other workers. When the worker's result arrives, call Finish with that word in the summary.`

// The live sub-agent check (docs/opencode-go.md, テスト方針): a Host with
// Codex registered on a server that must get nothing chooses Flash with
// /model-sub, and a lead on the real API starts a worker-low that reads one
// file. At most liveSubagentBudget Responses requests reach OpenCode Go;
// approvals the lead asks of the test are denied. It runs only with
// UNREAL_LIVE_OPENCODE_GO=1. The test never reads the key; the Host does.
func TestLiveOpenCodeGoSubagentsThroughHost(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_OPENCODE_GO") != "1" {
		t.Skip("set UNREAL_LIVE_OPENCODE_GO=1 to run a lead and a worker on an offered model with the stored key")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "unreal-agent")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "noted"})
	cfg := claudeRouteConfiguration(t, f)
	word := "live-" + strings.ToLower(rand.Text()[:8])
	if err = os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "live-word.txt"), []byte(word+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	proxy := newLiveOpenCodeGoProxy(t)
	endpoint := httptest.NewServer(proxy)
	t.Cleanup(endpoint.Close)
	model := liveModel()
	listed := modelcatalog.ReadOpenCodeGo(t.Context(), &http.Client{Timeout: 20 * time.Second}, endpoint.URL+"/inference/go/v1/models", opencodego.OfferedModels())
	if len(listed.Models) != len(opencodego.OfferedModels()) {
		t.Fatalf("the model list offers %d of %d models: %s", len(listed.Models), len(opencodego.OfferedModels()), listed.ProblemCode)
	}
	probed, e := listed.FindSelection(model)
	if e != nil {
		t.Fatalf("the model list has no %s: %s", model, listed.ProblemCode)
	}
	leadEffort, workerEffort, collectorEffort := subagentEffort("lead", probed), subagentEffort("worker-low", probed), subagentEffort("collector", probed)
	proxy.wantRule = "collector template (" + model
	if collectorEffort != "" {
		proxy.wantRule += " at " + string(collectorEffort) + " effort), never to a higher-effort worker"
	} else {
		proxy.wantRule += ")"
	}
	route := openCodeGoRoute(model)
	route.Endpoint = endpoint.URL + "/inference/go/openai/v1"
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: route, ReasoningEffort: "high"}}
	cfg.Permissions.NetworkOrigins = []string{endpoint.URL}
	codexCalls := withFakeCodex(t, &cfg, true)
	h := &openCodeGoChildren{gptHost: &gptHost{binary: binary, claude: f}, codexCalls: codexCalls, cfg: cfg, limit: 15 * time.Minute,
		store: filepath.Join(home, ".local", "state", "unreal-agent", "credentials"), modelFile: filepath.Join(privateCLIDirectory(t), "subagent-model.json")}
	h.start(t)
	h.designer(t)
	if chosen, err := h.choose(t, opencodego.ProviderID, model); err != nil || chosen.Pending {
		t.Fatal("choosing "+model, chosen, err)
	}

	started := h.mustCLI(t, liveLeadTask, "start", "--template", "lead")
	handle := childHandle.FindStringSubmatch(started)
	if handle == nil {
		t.Fatal(started)
	}
	approval := regexp.MustCompile(`approval needed: request (\S+): (.*)`)
	deadline := time.Now().Add(12 * time.Minute)
	for {
		out := h.mustCLI(t, "", "wait", handle[1], "--timeout", "30s")
		if proxy.overBudget() || time.Now().After(deadline) {
			h.cli(t, "", "cancel", handle[1])
			proxy.report(t)
			t.Fatal("the lead did not finish within the budget or the time\n", out)
		}
		asked := approval.FindAllStringSubmatch(out, -1)
		for _, a := range asked {
			t.Logf("denied the lead's request: %s", a[2])
			h.mustCLI(t, "", "approve", handle[1], a[1], "deny")
		}
		if len(asked) == 0 && !strings.Contains(out, "Still running.") {
			break
		}
	}
	report := h.mustCLI(t, "", "show", handle[1])
	t.Log(report)
	requests := proxy.report(t)
	if !strings.Contains(report, "result: completed") || !strings.Contains(report, word) {
		t.Fatal("the lead did not report the worker's word", word)
	}
	leadLabel := handle[1] + "  lead (" + model
	if leadEffort != "" {
		leadLabel += " " + string(leadEffort)
	}
	if list := h.mustCLI(t, "", "list", "--all"); !strings.Contains(list, leadLabel+")  completed") {
		t.Fatal(list)
	}
	// The lead at its highest offered effort and its worker-low at the lowest,
	// all on the chosen model with the key.
	roles := map[string]int{}
	for i, r := range requests {
		roles[r.role]++
		want, checked := map[string]string{"lead": string(leadEffort), "worker": string(workerEffort)}[r.role]
		if r.model != model || !r.key || checked && r.effort != want {
			t.Error("request", i+1, r.role, r.model, r.effort, r.key)
		}
	}
	if roles["lead"] == 0 || roles["worker"] == 0 {
		t.Fatal("requests by role", roles)
	}
	proxy.mu.Lock()
	leadRule, codexModels := proxy.leadRule, proxy.codexModels
	proxy.mu.Unlock()
	if !leadRule || codexModels {
		t.Fatal("the lead's prompt does not name the chosen model and its collector effort", leadRule, codexModels)
	}
	if n := codexCalls.Load(); n != 0 {
		t.Fatal("Codex was used", n)
	}
}
