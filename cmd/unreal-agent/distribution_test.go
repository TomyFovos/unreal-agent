//go:build linux && cgo

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
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
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func snapshotExecutable(t *testing.T, name string) string {
	t.Helper()
	directory := os.Getenv("UNREAL_SNAPSHOT_BIN_DIR")
	if directory == "" {
		t.Skip("set UNREAL_SNAPSHOT_BIN_DIR to an inspected, extracted snapshot")
	}
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("missing snapshot executable %s", name)
	}
	return path
}

func TestSnapshotPackagedHostNoInference(t *testing.T) {
	binary := snapshotExecutable(t, "unreal-agent")
	f := newLauncherFixture(t) // private paths and synthetic Codex credentials
	// testing.T cancels its context before cleanups. Own the process until
	// SIGTERM has drained the Host instead of killing it at that boundary.
	processContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	process := exec.CommandContext(processContext, binary, "serve", "--config", f.o.config,
		"--session-directory", f.o.sessions, "--socket", f.o.socket,
		"--codex-auth-file", f.o.codexFile)
	var output credentialOutput
	process.Stdout, process.Stderr = &output, &output
	if err := process.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		defer cancel()
		if err := process.Process.Signal(syscall.SIGTERM); err != nil {
			t.Error("snapshot Host exited before shutdown", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("snapshot Host shutdown", err, output.String())
			}
		case <-time.After(5 * time.Second):
			process.Process.Kill()
			<-done
			t.Error("snapshot Host did not drain")
		}
	})
	client := gateway.NewClient(f.o.socket)
	defer client.Close()
	waitInteractive(t, func() bool { return probeHost(t.Context(), client) == nil })
	view, err := client.Open(t.Context(), host.Create, "snapshot-smoke")
	if err != nil || !view.Running {
		t.Fatal("snapshot Host could not create an idle Session", err)
	}
	for range 2 {
		subscription, err := client.Subscribe(t.Context(), "snapshot-smoke", 0, 256, 16)
		if err != nil {
			t.Fatal("snapshot attach failed", err)
		}
		subscription.Cancel() // detach the projection without stopping the Host
	}
	view, err = client.Inspect(t.Context(), "snapshot-smoke", 0, 256)
	if err != nil || !view.Running || len(view.Operations) != 0 || f.calls.Load() != 0 {
		t.Fatal("idle attach/detach changed lifecycle or invoked inference", err)
	}
}

func TestSnapshotPackagedRunnerAST(t *testing.T) {
	binary := snapshotExecutable(t, "unreal-agent-runner")
	root := privateCLIDirectory(t)
	source := "package p\nfunc snapshot_value() {}\n"
	path := filepath.Join(root, "sample.go")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var observed atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		var output []map[string]any
		switch calls.Add(1) {
		case 1:
			arguments, _ := json.Marshal(map[string]any{
				"language": "go", "query": "(identifier) @match",
				"targets": []map[string]string{{"path": "sample.go"}},
			})
			output = []map[string]any{{"id": "fc", "type": "function_call", "call_id": "ast-call",
				"name": tool.ASTGrepName, "arguments": string(arguments), "status": "completed"}}
		case 2:
			observed.Store(bytes.Contains(body, []byte("snapshot_value")) && bytes.Contains(body, []byte("function_call_output")))
			output = []map[string]any{{"id": "message", "type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]string{{"type": "output_text", "text": "snapshot AST verified"}}}}
		default:
			t.Error("snapshot runner unexpectedly requested another fake generation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		frame, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "response", "object": "response", "status": "completed", "output": output,
		}})
		io.WriteString(w, "data: "+string(frame)+"\n\n")
	}))
	defer endpoint.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, binary, "-workspace", root, "-session-directory", filepath.Join(root, "sessions"))
	process.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root,
		"UNREAL_HARNESS_LLM_PROVIDER=openai", "UNREAL_HARNESS_LLM_MODEL=synthetic-model",
		"UNREAL_HARNESS_LLM_API_KEY=synthetic-key", "UNREAL_HARNESS_LLM_BASE_URL=" + endpoint.URL,
		"UNREAL_HARNESS_LLM_MAX_ATTEMPTS=1",
	}
	process.Stdin = strings.NewReader(`{"prompt":"Inspect the Go identifiers without changing files."}`)
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	if err := process.Run(); err != nil {
		t.Fatal("packaged runner AST smoke failed", err, stderr.String())
	}
	if calls.Load() != 2 || !observed.Load() || !strings.Contains(stdout.String(), "snapshot AST verified") {
		t.Fatal("packaged AST result did not reach the next fake generation")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != source {
		t.Fatal("AST preview modified the fixture", err)
	}
}
