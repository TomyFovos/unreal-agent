//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// With UNREAL_COMMON_BIN_DIR this runs the exact final distribution Host. The
// only endpoints/authentication/CLI files are disposable synthetic fixtures.
func TestCommonProductRoutesPickerChoicesAndReceipts(t *testing.T) {
	for _, name := range []string{"asp-labelled-home-devlore-repo", "devlore-labelled-home-asp-repo"} {
		t.Run(name, func(t *testing.T) { t.Setenv("HOSTNAME", name); testCommonProductRoutes(t) })
	}
}

func testCommonProductRoutes(t *testing.T) {
	_, f := normalFixture(t, true, true)
	if os.Getenv("UNREAL_COMMON_KIT_DIRECTORY") != "" {
		// Keep the synthetic HOME short enough for the real Unix socket guard,
		// including the separate approved-update instance. Never relax it.
		home, err := os.MkdirTemp("/tmp", "uc-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(home) })
		t.Setenv("HOME", home)
		// Do not expose the operator's existing unreal/aliases to installation
		// fixtures. The installer's real command-conflict protection is required.
		t.Setenv("PATH", f.Directory+string(os.PathListSeparator)+"/usr/bin:/bin")
		if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
			t.Fatal(err)
		}
		writeCodexAuth(t, filepath.Join(home, ".codex", "auth.json"), "common-synthetic-token", "common-synthetic-account")
		if err := os.WriteFile(filepath.Join(home, ".codex", "models_cache.json"), []byte(normalCodexCache), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, AgentSteps: []testclaude.AgentStep{{Tool: "Read", Input: jsontext.Value(`{"file_path":"source.txt"}`)}}, AgentFinal: "Claude Code fixture completed"})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Input []jsontext.Value `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error("invalid synthetic request")
			return
		}
		if request.Model != "gpt-6.1-sol" || request.Reasoning.Effort != "high" {
			t.Error("picker did not route selected model/effort")
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"common-frame\",\"call_id\":\"common-read\",\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"source.txt\\\"}\"}]}}\n\n")
		} else {
			var found bool
			for _, raw := range request.Input {
				var item struct {
					Type   string         `json:"type"`
					Output jsontext.Value `json:"output"`
				}
				if json.Unmarshal(raw, &item) == nil && item.Type == "function_call_output" && item.Output.IsValid() && strings.Contains(string(item.Output), "COMMON_FILE_RECEIPT") {
					found = true
				}
			}
			if n == 2 && !found {
				t.Error("canonical native receipt absent from continuation")
			}
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"common Codex complete\"}]}]}}\n\n")
		}
	}))
	defer server.Close()
	o, err := parseLaunchOptions([]string{"--bootstrap-claude-binary", f.Binary}, io.Discard, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := bootstrapRuntime(t.Context(), o, func([]string) (string, error) { return "openai-codex", nil }, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	// Only the synthetic HTTP endpoint is injected. The tool set, budget,
	// permissions, auth owner, profile and discovery are normal defaults.
	cfg.Runtime.Provider.Endpoint = server.URL
	cfg.Permissions.NetworkOrigins = []string{server.URL}
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "AGENTS.md"), []byte("Read before editing; never commit or push.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Permissions.Tools = []string{"read", "grep", "glob"}
	cfg.Permissions.ReadRoots = []string{cfg.Runtime.Workspace}
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "source.txt"), []byte("COMMON_FILE_RECEIPT"), 0600); err != nil {
		t.Fatal(err)
	}
	directory := privateCLIDirectory(t)
	var c *gateway.Client
	binaryDirectory := os.Getenv("UNREAL_COMMON_BIN_DIR")
	var selected commonTestPaths
	if kit := os.Getenv("UNREAL_COMMON_KIT_DIRECTORY"); kit != "" {
		t.Skip("the kit update fixture modelled the removed claude-code route; installs are checked by the common kit tests")
	}
	if binaryDirectory != "" {
		if selected.Config != "" {
			c = startPackagedHostAt(t, cfg, selected.Config, selected.Socket, filepath.Join(selected.State, "sessions"), binaryDirectory)
		} else {
			c = startPackagedInteractiveHost(t, cfg, directory, binaryDirectory)
		}
	} else {
		c, _, _ = startClaudeStreamHost(t, cfg, directory)
	}
	v, err := c.Open(t.Context(), host.Create, "common-product")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.providers", ID: "common-product"})
	if err != nil || len(providers.Providers) != 2 {
		t.Fatal("same product missing provider catalog", err)
	}
	multiSelect(t, c, v, 0, "openai-codex", "gpt-6.1-sol", "high")
	submitInteractive(t, c, v, "codex-request", "Read the fixture and finish.")
	end := inspectInteractive(t, c, "common-product", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return responseCount(v) == 2
	})
	if calls.Load() != 2 || len(end.Operations) != 1 {
		t.Fatal("Codex selection/receipt path incorrect")
	}
	multiSelect(t, c, v, 1, claudeagent.ProviderID, "discovered-b", "high")
	submitInteractive(t, c, v, "claude-request", "Read the same fixture, then finish.")
	end = inspectInteractive(t, c, "common-product", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return hasFinalText(v, "Claude Code fixture completed")
	})
	// One Claude Code turn; its own Read is recorded as a canonical receipt.
	if len(end.Operations) != 2 || len(f.Calls(t)) != 1 || calls.Load() != 2 {
		t.Fatal("provider routing used fallback or missed canonical receipt")
	}
	// Actual ContextBuilt diagnostics passed through the Host and analysis,
	// rather than constructing a renderer fixture with the desired percentage.
	report := multiReport(t, end)
	// Context diagnostics are ephemeral in View, not canonical history. This is
	// the same assignment the native TUI makes in Model.Analysis.
	report.ContextPackage = end.Context
	d := end.Context
	if d == nil || d.Budget.Input <= 0 || math.Abs(d.Utilization-100*float64(d.EstimatedInputTokens)/float64(d.Budget.Input)) > 1e-10 || !strings.Contains(strings.Join(analysis.Lines(report, "Context", true), "\n"), fmt.Sprintf("%.1f%%", d.Utilization)) {
		t.Fatal("projected Report.Context -> ContextBuilt -> analysis percentage mismatch", d)
	}
	for _, call := range f.Calls(t) {
		cliArgument(t, call, "--model", "discovered-b")
		cliArgument(t, call, "--effort", "high")
	}
	selection := multiReport(t, end).Selection
	if selection == nil || selection.Provider != claudeagent.ProviderID || selection.Model != "discovered-b" || selection.Effort != "high" {
		t.Fatal("canonical selection differs from actual request")
	}
	multiSelect(t, c, v, 2, "openai-codex", "gpt-6.1-sol", "high")
	submitInteractive(t, c, v, "codex-return-request", "Return a final report from the already observed receipt; do not reexecute tools.")
	end = inspectInteractive(t, c, "common-product", func(view host.View) bool {
		if view.Failure != "" {
			t.Fatal(view.Failure)
		}
		return responseCount(view) == 5
	})
	finalSelection := multiReport(t, end).Selection
	if finalSelection == nil || finalSelection.Provider != "openai-codex" || finalSelection.Revision != 3 || calls.Load() != 3 || len(f.Calls(t)) != 1 || len(end.Operations) != 2 {
		t.Fatal("Codex→Claude→Codex did not pin runtime or replayed actions")
	}
	for range 2 {
		sub, err := c.Subscribe(t.Context(), "common-product", 0, 256, 16)
		if err != nil {
			t.Fatal(err)
		}
		sub.Cancel()
	}
	stopMultiSession(t, c, end)
	resumed, err := c.Open(t.Context(), host.Resume, "common-product")
	if err != nil || resumed.Generation == v.Generation || len(resumed.Operations) != 2 {
		t.Fatal("resume repeated or discarded Operations", err)
	}
	if len(f.Calls(t)) != 1 || calls.Load() != 3 {
		t.Fatal("resume replayed completed side effects")
	}
	t.Log(fmt.Sprintf("same product: Codex requests=%d Claude generations=%d canonical Operations=%d; no real inference", calls.Load(), len(f.Calls(t)), len(end.Operations)))
}

// Downgrade only a disposable synthetic launcher binding to the exact old
// schemas. This exercises their real atomic migration; no operator setting,
// Host or archive is edited and no old audit bool is passed to a native probe.
func commonSyntheticLegacyBinding(t *testing.T, claudeOnly bool) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".config/unreal-agent-common/launcher.json")
	raw, err := os.ReadFile(path)
	var settings map[string]jsontext.Value
	if err != nil || json.Unmarshal(raw, &settings) != nil {
		t.Fatal("synthetic old binding unavailable")
	}
	delete(settings, "TrustSource")
	version := jsontext.Value("1")
	if claudeOnly {
		version = jsontext.Value("2")
	} else {
		for _, key := range []string{"CodexRoute", "ClaudeDecision", "ProviderStatus"} {
			delete(settings, key)
		}
	}
	settings["Version"] = version
	b, err := json.Marshal(settings)
	if err != nil || os.WriteFile(path, b, 0600) != nil {
		t.Fatal("cannot publish disposable old binding")
	}
}

type commonTestPaths struct{ Config, State, Socket string }

func installCommonTestKit(t *testing.T, kit string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{filepath.Join(kit, "development.sh")}, args...)...)
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("isolated exact kit setup failed", err, string(output))
	}
}

func commonPathsForTest(t *testing.T, kit, workspace string, update bool) commonTestPaths {
	t.Helper()
	// An update selects the new instance without a question, as the
	// launcher does.
	code := `import json,sys
from pathlib import Path
sys.path.insert(0,sys.argv[1])
import common_development as c, daily_development as d
s=c.read_settings(d.home());w=Path(sys.argv[2])
if sys.argv[3]=='update': p=c.select_repo(s,w,None)
else: p=c.prepare_repo(s,w,'default')
print('SYNTHETIC_PATHS:'+json.dumps({k.title():str(p[k]) for k in ('config','state','socket')}))
`
	mode := "old"
	if update {
		mode = "update"
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-B", "-c", code, kit, workspace, mode)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("owned synthetic workspace selection failed", err, string(output))
	}
	var paths commonTestPaths
	for line := range strings.SplitSeq(string(output), "\n") {
		if strings.HasPrefix(line, "SYNTHETIC_PATHS:") && json.Unmarshal([]byte(strings.TrimPrefix(line, "SYNTHETIC_PATHS:")), &paths) != nil {
			t.Fatal("invalid synthetic path reply")
		}
	}
	if paths.Config == "" || paths.State == "" || paths.Socket == "" {
		t.Fatal("missing synthetic owned paths")
	}
	return paths
}

func startCommonOldSocket(t *testing.T, socket string) *os.Process {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "python3", "-B", "-c", "import socket,sys,time;s=socket.socket(socket.AF_UNIX);s.bind(sys.argv[1]);s.listen();print('ready',flush=True);time.sleep(120)", socket)
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		cancel()
		t.Fatal("cannot start test-owned old socket")
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal("old synthetic Host not ready")
	}
	return cmd.Process
}

// Start only a test-owned, deadline-bounded distribution Host and reap it.
func startPackagedInteractiveHost(t *testing.T, cfg serveConfiguration, directory, binaryDirectory string) *gateway.Client {
	t.Helper()
	return startPackagedHostAt(t, cfg, filepath.Join(directory, "config.json"), filepath.Join(directory, "host.sock"), filepath.Join(directory, "sessions"), binaryDirectory)
}

func startPackagedHostAt(t *testing.T, cfg serveConfiguration, path, socket, store, binaryDirectory string) *gateway.Client {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0700) != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("synthetic config write failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, filepath.Join(binaryDirectory, "unreal-agent"), "serve", "--config", path, "--session-directory", store, "--socket", socket)
	cmd.Env = os.Environ()
	var output credentialOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		defer cancel()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Error("isolated packaged Host failed", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("packaged Host did not reap")
		}
	})
	client := gateway.NewClient(socket)
	t.Cleanup(func() { _ = client.Close() })
	waitInteractiveContext(t, func(ctx context.Context) bool { return probeHost(ctx, client) == nil })
	return client
}
