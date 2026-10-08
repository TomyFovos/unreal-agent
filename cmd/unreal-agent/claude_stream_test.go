//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

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
	waitInteractive(t, func() bool {
		select {
		case <-done:
			t.Fatalf("serve startup failed: %v", serveErr)
		default:
		}
		_, err := c.Methods(t.Context())
		return err == nil
	})
	return c, stop, output
}

const textMetadataStream = `{"type":"system","subtype":"init","model":"wire-a","tools":[],"mcp_servers":[],"permissionMode":"default"}
{"type":"system","subtype":"api_retry","attempt":1,"max_retries":3,"retry_delay_ms":0,"error_status":null,"error":"overloaded","uuid":"metadata-uuid-sensitive","session_id":"metadata-session-sensitive"}
{"type":"system","subtype":"thinking_tokens","estimated_tokens":2,"estimated_tokens_delta":2,"uuid":"metadata-uuid-sensitive","session_id":"metadata-session-sensitive"}
{"type":"system","subtype":"informational","content":"metadata-body-sensitive","level":"notice","uuid":"metadata-uuid-sensitive","session_id":"metadata-session-sensitive"}
{"type":"system","subtype":"notification","key":"metadata-key-sensitive","text":"metadata-body-sensitive","priority":"medium","uuid":"metadata-uuid-sensitive","session_id":"metadata-session-sensitive"}
{"type":"system","subtype":"session_state_changed","state":"running","uuid":"metadata-uuid-sensitive","session_id":"metadata-session-sensitive"}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"offline response"}}}
{"type":"assistant","message":{"id":"msg_offline","model":"wire-a","content":[{"type":"text","text":"offline response"}]}}
{"type":"result","subtype":"success","usage":{"input_tokens":10,"output_tokens":4}}
`

func TestClaudeRefreshModelInferenceAndFullHostRestartKeepStreamsSeparate(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	config.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	gate := filepath.Join(f.Directory, "inference-ready")
	fake := testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, Gate: gate, Stream: textMetadataStream}
	f.Set(t, fake)
	directory := privateCLIDirectory(t)
	c, stop, output := startClaudeStreamHost(t, config, directory)
	v, err := c.Open(t.Context(), host.Create, "stream-isolation")
	if err != nil {
		t.Fatal(err)
	}
	refresh := func(c *gateway.Client) {
		t.Helper()
		out, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "stream-isolation", Refresh: true})
		if err != nil || out.Catalog == nil || !out.Catalog.Authoritative || len(out.Catalog.Models) != 3 {
			t.Fatal("discovered catalog/refresh failed", err)
		}
	}
	refresh(c)
	if len(f.Calls(t)) != 0 {
		t.Fatal("catalog performed inference")
	}
	submitInteractive(t, c, v, "first", "offline first")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 1 })
	// Refresh independently while the already initialized text subprocess waits.
	refresh(c)
	if len(f.CatalogCalls(t)) != 2 || len(f.Calls(t)) != 1 {
		t.Fatal("refresh reused/continued inference")
	}
	if err = os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	first := inspectInteractive(t, c, "stream-isolation", func(v host.View) bool { return responseCount(v) == 1 || v.Failure != "" })
	if first.Failure != "" || len(first.Operations) != 0 {
		t.Fatal("metadata inference failed or created tools", first.Failure)
	}
	if _, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "stream-isolation", Generation: v.Generation, Selection: sessionstore.RuntimeSelection{Version: 1, RequestID: "stream-model-choice", Model: "discovered-b", Effort: "xhigh"}}); err != nil {
		t.Fatal(err)
	}
	fake.Gate, fake.Stream = "", strings.ReplaceAll(textMetadataStream, "wire-a", "wire-b")
	f.Set(t, fake)
	refresh(c)
	submitInteractive(t, c, v, "second", "offline second")
	second := inspectInteractive(t, c, "stream-isolation", func(v host.View) bool { return responseCount(v) == 2 || v.Failure != "" })
	if second.Failure != "" || len(second.Operations) != 0 {
		t.Fatal("model switch then inference failed", second.Failure)
	}
	cliArgument(t, f.Calls(t)[1], "--model", "discovered-b")
	cliArgument(t, f.Calls(t)[1], "--effort", "xhigh")
	if _, err = c.Stop(t.Context(), "stream-isolation", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	stopped := inspectInteractive(t, c, "stream-isolation", func(v host.View) bool { return !v.Running })
	completed := false
	for _, item := range stopped.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == "stop_complete" {
			completed = true
		}
	}
	if !completed {
		t.Fatal("stop completion did not remain canonical")
	}
	stop()
	c2, _, output2 := startClaudeStreamHost(t, config, directory)
	resumed, err := c2.Open(t.Context(), host.Resume, "stream-isolation")
	if err != nil {
		t.Fatal(err)
	}
	refresh(c2)
	if len(f.Calls(t)) != 2 {
		t.Fatal("resume/refresh performed speculative inference")
	}
	submitInteractive(t, c2, resumed, "third", "offline third")
	third := inspectInteractive(t, c2, "stream-isolation", func(v host.View) bool { return responseCount(v) == 3 || v.Failure != "" })
	if third.Failure != "" || len(third.Operations) != 0 {
		t.Fatal("resume refresh inference failed", third.Failure)
	}
	cliArgument(t, f.Calls(t)[2], "--model", "discovered-b")
	acc := analysis.New("stream-isolation")
	acc.Created(third.Session.Session.CreatedAt)
	for _, item := range third.History.Items {
		acc.Apply(item)
	}
	report := acc.Snapshot(time.Now(), third.Running, false, nil, third.Operations)
	if report.Selection == nil || report.Selection.Model != "discovered-b" || len(report.Turns) != 3 {
		t.Fatal("analysis lost canonical selection/turns")
	}
	data, err := json.Marshal(third.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{string(data), output.String(), output2.String()} {
		if strings.Contains(source, "metadata-") || strings.Contains(source, "control_response") || strings.Contains(source, "account-sensitive") {
			t.Fatal("private metadata/control stream entered Host state")
		}
	}
	for _, format := range []string{"JSON", "Markdown"} {
		data, err := analysis.Encode(report, format)
		if err != nil || strings.Contains(string(data), "metadata-") {
			t.Fatal("metadata entered analysis/export", err)
		}
	}
	for _, catalog := range f.CatalogCalls(t) {
		for _, inference := range f.Calls(t) {
			if catalog.PID == inference.PID || catalog.Directory == inference.Directory {
				t.Fatal("process/cwd reused between protocols")
			}
		}
		if err := syscall.Kill(catalog.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatal("catalog child not reaped", err)
		}
		if _, err := os.Stat(catalog.Directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("catalog cwd not removed", err)
		}
	}
}
