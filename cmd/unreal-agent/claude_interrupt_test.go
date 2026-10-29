//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Renderer writes complete changed rows. Keep the current screen, not old
// pulse bytes that have already been erased from the terminal.
type interruptScreen struct {
	mu   sync.Mutex
	rows map[string]string
}

func TestClaudeIdleCompactionYieldsToTUIInputAndRetries(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentInputTokens: 250_000, AgentFinal: "first complete", CompactDelayMillis: 30_000})
	cfg := claudeRouteConfiguration(t, f)
	c, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
	v, err := c.Open(t.Context(), host.Create, "idle-interrupt")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	keys, screen, done := make(chan tui.Key, 16), &interruptScreen{}, make(chan error, 1)
	go func() {
		done <- tui.Run(ctx, tui.Config{Client: c, ID: "idle-interrupt", Keys: keys, Output: screen, Theme: &tui.Theme{Plain: true}, Size: func() (int, int) { return 160, 40 }})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TUI did not stop")
		}
	}()
	submitInteractive(t, c, v, "first", "first task")
	inspectInteractive(t, c, "idle-interrupt", func(v host.View) bool {
		return hasFinalText(v, "first complete") && v.Progress != nil && v.Progress.Mode == "compacting"
	})
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 2 })
	waitInteractive(t, func() bool { return strings.Count(screen.text(), "会話を圧縮中") >= 2 })
	// The already running fake keeps its 30s delay. Subsequent invocations
	// answer and compact immediately, making a retry independently observable.
	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentInputTokens: 250_000, AgentFinal: "second complete", CompactPreTokens: 250_050, CompactPostTokens: 12_000})
	submitted := time.Now()
	keys <- tui.Key{Text: "second task"}
	keys <- tui.Key{Name: "enter"}
	end := inspectInteractive(t, c, "idle-interrupt", func(v host.View) bool {
		for _, item := range v.History.Items {
			if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Context != nil && record.Context.Conversation != nil && record.Context.Conversation.Compacted {
				return hasFinalText(v, "second complete") && v.Progress == nil
			}
		}
		return false
	})
	calls := f.Calls(t)
	if len(calls) != 4 || !strings.HasPrefix(calls[1].Input, "/compact ") || calls[2].Input != "second task" || !strings.HasPrefix(calls[3].Input, "/compact ") {
		t.Fatalf("interrupt/retry process order = %+v", calls)
	}
	if elapsed := calls[2].InputAt.Sub(submitted); elapsed < 0 || elapsed >= 5*time.Second {
		t.Fatalf("input waited %v for a 30s compaction", elapsed)
	}
	for _, call := range calls {
		if call.OverlappingPID != 0 {
			t.Fatalf("Claude processes overlapped: %+v", call)
		}
	}
	interrupted, compacted := 0, 0
	for _, item := range end.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Context != nil && record.Context.Conversation != nil {
			r := record.Context.Conversation
			if r.Interrupted {
				interrupted++
				if r.Compacted || !r.InputKnown || r.InputTokens != 250_050 || r.BeforeTokens != 250_050 || r.Error != "" {
					t.Fatal("interruption changed the pre-compact context", r)
				}
			}
			if r.Compacted {
				compacted++
				if r.BeforeTokens != 250_050 || !r.InputKnown || r.InputTokens != 12_000 {
					t.Fatal("retry compact counts", r)
				}
			}
		}
	}
	if interrupted != 1 || compacted != 1 {
		t.Fatalf("interrupted=%d compacted=%d; want one of each", interrupted, compacted)
	}
}

func TestClaudeSteeringAtToolBatchDoesNotResendInput(t *testing.T) {
	f := testclaude.New(t)
	cfg := claudeRouteConfiguration(t, f)
	started, release := filepath.Join(cfg.Runtime.Workspace, "started"), filepath.Join(cfg.Runtime.Workspace, "release")
	input, _ := json.Marshal(map[string]string{"command": "touch '" + started + "'; while [ ! -f '" + release + "' ]; do sleep 0.01; done"})
	f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentOnce: true, AgentFinal: "steered turn finished", AgentSteps: []testclaude.AgentStep{{Tool: "Bash", Input: input}, {Tool: "Bash", Input: jsontext.Value(`{"command":"printf continued"}`)}}})
	c, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
	v, err := c.Open(t.Context(), host.Create, "steering")
	if err != nil {
		t.Fatal(err)
	}
	w, err := c.WatchPermissions(t.Context(), "steering", v.Generation)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Cancel()
	setGatewayHumanMode(t, w, permission.FullAccess)
	submitInteractive(t, c, v, "task", "Work through the tools.")
	waitInteractive(t, func() bool { _, err := os.Stat(started); return err == nil })
	submitInteractive(t, c, v, "steer-one", "進捗報告して\n貼り付けた次の行")
	submitInteractive(t, c, v, "steer-two", "Check this too. api_key=sk-sensitive12345\x1b[31m")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	inspectInteractive(t, c, "steering", func(v host.View) bool { return hasFinalText(v, "steered turn finished") })
	contexts := f.BatchContexts(t)
	if len(contexts) != 2 || !strings.Contains(contexts[0], "進捗報告して") || !strings.Contains(contexts[0], "貼り付けた次の行") || !strings.Contains(contexts[0], "Check this too.") || !strings.Contains(contexts[0], "Answer the user first") || contexts[1] != "" {
		t.Fatalf("next PostToolBatch did not consume both inputs once: %#v", contexts)
	}
	if strings.Contains(contexts[0], "sk-sensitive12345") || strings.Contains(contexts[0], "\x1b") {
		t.Fatal("unsafe steering context")
	}
	// A new input forces the next request, making an unnoticed duplicate turn
	// or a duplicate message in its resumed prompt observable.
	submitInteractive(t, c, v, "next", "Now answer this new question.")
	inspectInteractive(t, c, "steering", func(v host.View) bool { return len(f.Calls(t)) >= 2 && hasFinalText(v, "steered turn finished") })
	stopMultiSession(t, c, v)
	calls := f.Calls(t)
	if len(calls) != 2 || !strings.Contains(calls[1].Input, "Now answer this new question.") || strings.Contains(calls[1].Input, "進捗報告して") || strings.Contains(calls[1].Input, "Check this too.") {
		t.Fatalf("steering was resent after the turn: calls=%d next=%q", len(calls), calls[len(calls)-1].Input)
	}
}

var interruptRow = regexp.MustCompile("\x1b\\[([0-9]+);1H([^\x1b]*)\x1b\\[K")

func (s *interruptScreen) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]string{}
	}
	for _, m := range interruptRow.FindAllStringSubmatch(string(data), -1) {
		s.rows[m[1]] = m[2]
	}
	return len(data), nil
}
func (s *interruptScreen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []string
	for _, row := range s.rows {
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func TestClaudeStopMainKeepsTUIResponsive(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused=%t", refused), func(t *testing.T) {
			f := testclaude.New(t)
			cfg := claudeRouteConfiguration(t, f)
			marker := filepath.Join(cfg.Runtime.Workspace, "bash-started")
			input, _ := json.Marshal(map[string]string{"command": "touch '" + marker + "'; sleep 120"})
			f.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentSteps: []testclaude.AgentStep{{Tool: "Bash", Input: input, Refused: refused}}, AgentOnce: true, AgentFinal: "NEXT USER ANSWER"})
			c, _, _ := startInteractiveHost(t, cfg, "--normal-runtime")
			v, err := c.Open(t.Context(), host.Create, "stop-main")
			if err != nil {
				t.Fatal(err)
			}
			w, err := c.WatchPermissions(t.Context(), "stop-main", v.Generation)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Cancel()
			setGatewayHumanMode(t, w, permission.FullAccess)
			ctx, cancel := context.WithCancel(t.Context())
			keys, out, done := make(chan tui.Key, 16), &interruptScreen{}, make(chan error, 1)
			go func() {
				done <- tui.Run(ctx, tui.Config{Client: c, ID: "stop-main", Keys: keys, Output: out, Theme: &tui.Theme{Plain: true}, Size: func() (int, int) { return 100, 30 }})
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("TUI did not stop")
				}
			}()
			waitInteractive(t, func() bool { return strings.Contains(out.text(), "Ask") })
			keys <- tui.Key{Text: "Run the long command."}
			keys <- tui.Key{Name: "enter"}
			waitInteractive(t, func() bool { _, err := os.Stat(marker); return err == nil })
			keys <- tui.Key{Text: "/stop main"}
			keys <- tui.Key{Name: "enter"}
			inspectInteractive(t, c, "stop-main", func(v host.View) bool {
				for _, item := range v.History.Items {
					if in, ok := item.Data.(inbox.Input); ok {
						if control, err := in.DecodeControlMessage(); err == nil && control.Mode == inbox.InterruptAgent {
							return true
						}
					}
				}
				return false
			})
			waitInteractive(t, func() bool {
				screen := out.text()
				return !strings.Contains(screen, "応答待ち") && !strings.Contains(screen, "生成中") && !strings.Contains(screen, "考え中") && !strings.Contains(screen, "実行中") && !strings.Contains(screen, "sending")
			})
			keys <- tui.Key{Text: "Answer the next user input."}
			keys <- tui.Key{Name: "enter"}
			inspectInteractive(t, c, "stop-main", func(v host.View) bool { return hasFinalText(v, "NEXT USER ANSWER") })
			waitInteractive(t, func() bool { return strings.Contains(out.text(), "NEXT USER ANSWER") })
			calls := f.Calls(t)
			if len(calls) != 2 || !strings.Contains(calls[1].Input, "Answer the next user input.") {
				t.Fatalf("next input did not reach Claude: calls=%d", len(calls))
			}
		})
	}
}
