//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
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

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// With UNREAL_COMMON_BIN_DIR this runs the exact final distribution Host. The
// only endpoints/authentication/CLI files are disposable synthetic fixtures.
func TestCommonProductRoutesPickerChoicesAndReceipts(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"source.txt"}`, Contains: "COMMON_FILE_RECEIPT"}}})
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
			if !found {
				t.Error("canonical native receipt absent from continuation")
			}
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"common Codex complete\"}]}]}}\n\n")
		}
	}))
	defer server.Close()
	cfg := multiConfiguration(t, f, server.URL, true)
	cfg.Permissions.Tools = []string{"read", "grep", "glob"}
	cfg.Permissions.ReadRoots = []string{cfg.Runtime.Workspace}
	p := cfg.Providers["claude-code"]
	p.ClaudeCode.ToolBridge = claudecode.ToolBridgeConfig{Enabled: true, Mode: claudecode.BridgeModeStructured}
	cfg.Providers["claude-code"] = p
	if err := os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "source.txt"), []byte("COMMON_FILE_RECEIPT"), 0600); err != nil {
		t.Fatal(err)
	}
	directory := privateCLIDirectory(t)
	var c *gateway.Client
	if binaryDirectory := os.Getenv("UNREAL_COMMON_BIN_DIR"); binaryDirectory != "" {
		raw, _ := json.Marshal(cfg)
		path := filepath.Join(directory, "config.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		socket := filepath.Join(directory, "host.sock")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, filepath.Join(binaryDirectory, "unreal-agent"), "serve", "--config", path, "--session-directory", filepath.Join(directory, "sessions"), "--socket", socket)
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
		c = gateway.NewClient(socket)
		t.Cleanup(func() { _ = c.Close() })
		waitInteractive(t, func() bool { return probeHost(t.Context(), c) == nil })
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
	multiSelect(t, c, v, 1, "claude-code", "discovered-b", "high")
	submitInteractive(t, c, v, "claude-request", "Read the same fixture using Unreal, then finish.")
	end = inspectInteractive(t, c, "common-product", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal(v.Failure)
		}
		return hasBridgeFinal(v)
	})
	if len(end.Operations) != 2 || len(f.Calls(t)) != 2 || calls.Load() != 2 {
		t.Fatal("provider routing used fallback or missed canonical receipt")
	}
	for _, call := range f.Calls(t) {
		cliArgument(t, call, "--model", "discovered-b")
		cliArgument(t, call, "--effort", "high")
		cliArgument(t, call, "--tools", "")
	}
	selection := multiReport(t, end).Selection
	if selection == nil || selection.Provider != "claude-code" || selection.Model != "discovered-b" || selection.Effort != "high" {
		t.Fatal("canonical selection differs from actual request")
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
	if len(f.Calls(t)) != 2 || calls.Load() != 2 {
		t.Fatal("resume replayed completed side effects")
	}
	t.Log(fmt.Sprintf("same product: Codex requests=%d Claude generations=%d canonical Operations=%d; no real inference", calls.Load(), len(f.Calls(t)), len(end.Operations)))
}
