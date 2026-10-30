package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

type startupTimingScreen struct {
	frame, input chan time.Time
	mu           sync.Mutex
	output       bytes.Buffer
}

func (s *startupTimingScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.output.Write(p)
	s.mu.Unlock()
	if bytes.Contains(p, []byte("startup-entry-1999")) {
		select {
		case s.frame <- time.Now():
		default:
		}
	}
	if bytes.Contains(p, []byte("startup-input-probe")) {
		select {
		case s.input <- time.Now():
		default:
		}
	}
	return len(p), nil
}

// Real launcher, separate Host process, gateway subscriptions, viewer and TUI.
// The endpoint and every log are synthetic; startup must perform no inference.
func TestLargeSessionLauncherStartup(t *testing.T) {
	f := newLauncherFixture(t)
	if path := os.Getenv("UA_STARTUP_PROFILE"); path != "" {
		t.Setenv("UA_LAUNCHER_CPU_PROFILE", path)
	}
	providers, err := runtimeProviders(f.config.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Context: f.config.Context, Identity: f.config.Runtime, Providers: providers, SessionDirectory: f.o.sessions, Credentials: withExternalCodexCredentials(nil, filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"))})
	if err != nil {
		t.Fatal(err)
	}
	identity, err = withSubagentIdentity(identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.o.sessions, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range testsession.StartupLogs(t, f.o.sessions, identity) {
		if err := os.WriteFile(filepath.Join(f.o.sessions, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Timing is logged, not asserted: under -race with parallel packages
	// startup takes several times longer. go test's timeout catches a hang.
	ctx := t.Context()
	keys := make(chan tui.Key, 2)
	out := &startupTimingScreen{frame: make(chan time.Time, 1), input: make(chan time.Time, 1)}
	t.Cleanup(func() {
		if t.Failed() {
			out.mu.Lock()
			t.Log(out.output.String())
			out.mu.Unlock()
			log, _ := os.ReadFile(filepath.Join(f.o.state, "host.log"))
			t.Log(string(log))
		}
	})
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- f.run(ctx, []string{"main"}, func(ctx context.Context, client *gateway.Client, id session.ID) error {
			remote := viewer.Remote{ParentID: id, Parent: client, Call: client.Extension}
			panel := viewer.NewPanel(ctx, id, viewer.NewClient(remote, viewer.New(viewer.SubagentOptions()), remote))
			defer panel.Close()
			return tui.Run(ctx, tui.Config{Client: client, ID: id, Keys: keys, Output: out, Size: func() (int, int) { return 140, 32 }, Theme: &tui.Theme{Plain: true}, Viewer: panel.Snapshot, Updates: panel.Updates()})
		})
	}()
	wait := func(signal <-chan time.Time) time.Time {
		t.Helper()
		select {
		case at := <-signal:
			return at
		case err := <-done:
			t.Fatalf("launcher exited before display/input: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return time.Time{}
	}
	frame := wait(out.frame)
	keys <- tui.Key{Text: "startup-input-probe"}
	typed := wait(out.input)
	keys <- tui.Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("startup replayed inference", f.calls.Load())
	}
	t.Logf("launcher to first history frame: %s; to editable input: %s", frame.Sub(start), typed.Sub(start))
	log, err := os.ReadFile(filepath.Join(f.o.state, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Host phases:\n%s", log)
}
