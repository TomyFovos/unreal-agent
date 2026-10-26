//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Shared fixtures for the Claude and Codex Host tests.

const hostClaudeCatalog = `[
 {"value":"claude-configured-a","resolvedModel":"wire-a","displayName":"Discovered A","supportsEffort":true,"supportedEffortLevels":["low","medium"]},
 {"value":"discovered-b","resolvedModel":"wire-b","displayName":"Discovered B","supportsEffort":true,"supportedEffortLevels":["high","xhigh","unknown"]},
 {"value":"no-effort","resolvedModel":"wire-no-effort","displayName":"No effort model","supportsEffort":false,"supportedEffortLevels":[]}
]`

type multiWire struct {
	Model        string `json:"model"`
	Tools        []any  `json:"tools"`
	Input        []any  `json:"input"`
	Instructions string `json:"instructions"`
}

func cliArgument(t *testing.T, call testclaude.Call, name, want string) {
	t.Helper()
	i := slices.Index(call.Arguments, name)
	if i < 0 || i+1 >= len(call.Arguments) || call.Arguments[i+1] != want {
		t.Fatalf("wrong %s selection; expected %s", name, want)
	}
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

func liveAwait(t *testing.T, c *gateway.Client, id session.ID, timeout time.Duration) (host.View, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.Inspect(t.Context(), id, 0, 256)
		if err != nil {
			t.Fatal("inspect live session:", err)
		}
		if v.Failure != "" {
			return v, ""
		}
		if text, ok := liveFinal(v); ok {
			return v, text
		}
		if time.Now().After(deadline) {
			t.Fatal("live session did not finish in time")
		}
		time.Sleep(2 * time.Second)
	}
}

func liveFinal(v host.View) (string, bool) {
	for _, op := range v.Operations {
		if op.Status != operation.StatusCompleted && op.Status != operation.StatusFailed && op.Status != operation.StatusCanceled {
			return "", false
		}
	}
	var last *sessionstore.ModelResponse
	for _, item := range v.History.Items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok {
			last = &r
		}
	}
	if last == nil {
		return "", false
	}
	text := ""
	for _, output := range last.Response.Output {
		switch data := output.Data.(type) {
		case llm.ToolCall:
			return "", false
		case llm.Message:
			text += data.Text
		}
	}
	return text, text != ""
}

// Unlike a stopped Session alone, this helper joins the complete old Host before
// restarting it. No real executable or backend is used.
func startClaudeStreamHost(t *testing.T, config serveConfiguration, directory string) (*gateway.Client, func(), *credentialOutput) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "host.sock")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	output := &credentialOutput{}
	var serveErr error
	go func() {
		serveErr = run(ctx, []string{"serve", "--config", path, "--session-directory", filepath.Join(directory, "sessions"), "--socket", socket}, output)
		close(done)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
				if serveErr != nil {
					t.Errorf("serve failed: %v", serveErr)
				}
			case <-time.After(5 * time.Second):
				t.Error("old Host did not drain")
			}
		})
	}
	t.Cleanup(stop)
	c := gateway.NewClient(socket)
	t.Cleanup(func() { _ = c.Close() })
	waitInteractiveContext(t, func(ctx context.Context) bool {
		select {
		case <-done:
			t.Fatalf("serve startup failed: %v", serveErr)
		default:
		}
		_, err := c.Methods(ctx)
		return err == nil
	})
	return c, stop, output
}

func startMultiChildCLI(t *testing.T, binary string, cfg serveConfiguration, directory string, lifetime ...time.Duration) (*gateway.Client, func()) {
	t.Helper()
	raw, e := json.Marshal(cfg)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(directory, "config.json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	limit := 30 * time.Second
	if len(lifetime) > 0 {
		limit = lifetime[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "serve", "--config", path, "--session-directory", filepath.Join(directory, "sessions"), "--socket", filepath.Join(directory, "host.sock"))
	var diagnostics credentialOutput
	cmd.Stdout = &diagnostics
	cmd.Stderr = &diagnostics
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	var failure error
	go func() { failure = cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
				if failure != nil {
					known, code := interactiveExitCode(failure)
					t.Errorf("fake CLI Host phase=host_stop exit_code_known=%t exit_code=%d", known, code)
				}
			case <-time.After(5 * time.Second):
				t.Error("child Host did not drain")
			}
		})
	}
	t.Cleanup(stop)
	client := gateway.NewClient(filepath.Join(directory, "host.sock"))
	t.Cleanup(func() { client.Close() })
	waitInteractiveContext(t, func(ctx context.Context) bool {
		select {
		case <-done:
			known, code := interactiveExitCode(failure)
			t.Fatalf("CLI startup phase=host_startup exit_code_known=%t exit_code=%d", known, code)
		default:
		}
		_, e := client.Methods(ctx)
		return e == nil
	})
	return client, stop
}

func interactiveExitCode(err error) (bool, int) {
	if err == nil {
		return true, 0
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		code := exited.ExitCode()
		if code >= 0 && code <= 255 {
			return true, code
		}
	}
	return false, 0
}

func hasBridgeFinal(v host.View) bool {
	for _, i := range v.History.Items {
		if r, ok := i.Data.(sessionstore.ModelResponse); ok {
			for _, output := range r.Response.Output {
				if m, ok := output.Data.(llm.Message); ok && m.Text == "Bridge fixture completed" {
					return true
				}
			}
		}
	}
	return false
}
