//go:build linux || darwin

package claudeagent

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func idleProcessConfig(t *testing.T, fake *testclaude.Fixture) Config {
	t.Helper()
	return Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults(), Environ: func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fake.Home}
	}}
}

func TestIdleCompactionRespondDisarmsAcrossClients(t *testing.T) {
	fake := testclaude.New(t)
	fake.Set(t, testclaude.Config{AgentInputTokens: 250_000})
	_, opt, closeOwner := idleCompactionFixture(t, 250_000)
	defer closeOwner()
	opt.Tools = func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil }
	client, err := NewClient(idleProcessConfig(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("first")}}, opt)
	if err != nil {
		t.Fatal(err)
	}
	client.Idle(t.Context(), llm.Request{}, opt)
	session := sessionUUID("", opt.Identity.SessionID)
	key := client.config.Directory + "\x00" + session
	idleCompactions.Lock()
	state := idleCompactions.entries[key]
	armed := state != nil && state.pending != nil
	idleCompactions.Unlock()
	if !armed {
		t.Fatal("Idle did not arm a timer")
	}
	_ = client.Close()
	next, err := NewClient(client.config)
	if err != nil {
		t.Fatal(err)
	}
	input := append([]llm.Item{user("first")}, first.Output...)
	input = append(input, user("next turn"))
	if _, err := next.Respond(t.Context(), llm.Request{Input: input}, opt); err != nil {
		t.Fatal(err)
	}
	idleCompactions.Lock()
	disarmed := state.pending == nil && state.turns == 0 && state.armed != state.generation
	idleCompactions.Unlock()
	if !disarmed {
		t.Fatal("Respond left the previous Client's timer armed")
	}
	calls := fake.Calls(t)
	if len(calls) != 2 || calls[1].Input != "next turn" {
		t.Fatal("unexpected compact or duplicate turn", len(calls))
	}
}

type idleProcessObserver struct {
	*Client
	idles chan llm.RequestOptions
}

func (c *idleProcessObserver) Idle(ctx context.Context, r llm.Request, opt llm.RequestOptions) {
	c.Client.Idle(ctx, r, opt)
	c.idles <- opt
}

func waitIdleProcessCalls(t *testing.T, fake *testclaude.Fixture, count int) []testclaude.Call {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		calls := fake.Calls(t)
		if len(calls) >= count {
			return calls
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for compact process", len(calls))
		case <-tick.C:
		}
	}
}

func TestIdleCompactionHostCloseReapsPendingAndRunning(t *testing.T) {
	for _, phase := range []string{"timer", "compacting"} {
		t.Run(phase, func(t *testing.T) {
			fake := testclaude.New(t)
			fake.Set(t, testclaude.Config{AgentInputTokens: 250_000, CompactDelayMillis: 30_000})
			client, err := NewClient(idleProcessConfig(t, fake))
			if err != nil {
				t.Fatal(err)
			}
			model := &idleProcessObserver{Client: client, idles: make(chan llm.RequestOptions, 2)}
			h, err := host.New(t.Context(), host.Config{Directory: t.TempDir(), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
				return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: model, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx), Close: client.Close}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			compactPID := 0
			t.Cleanup(func() {
				if compactPID != 0 {
					_ = syscall.Kill(-compactPID, syscall.SIGKILL)
				}
				_ = h.Close()
			})
			s, err := h.Create(t.Context(), host.Options{ID: "idle-host", Initial: []inbox.Input{{ID: "start", Kind: inbox.InputExternal, Payload: []byte(`"start"`)}}})
			if err != nil {
				t.Fatal(err)
			}
			var opt llm.RequestOptions
			select {
			case opt = <-model.idles:
			case <-time.After(5 * time.Second):
				t.Fatal("Host never entered Idle")
			}
			key := client.config.Directory + "\x00" + sessionUUID("", string(s.ID))
			idleCompactions.Lock()
			state := idleCompactions.entries[key]
			armed := state != nil && state.pending != nil
			if armed && phase == "compacting" {
				// Advance only this timer; the virtual-clock tests cover 50 minutes.
				state.pending.due = time.Now()
				state.notify()
			}
			idleCompactions.Unlock()
			if !armed || opt.StartBackground == nil {
				t.Fatal("Host did not pass a live background owner to Idle")
			}
			if phase == "compacting" {
				calls := waitIdleProcessCalls(t, fake, 2)
				if calls[1].Input != compactPrompt || calls[1].OverlappingPID != 0 {
					t.Fatal("compact did not resume exclusively", calls[1].Input, calls[1].OverlappingPID)
				}
				compactPID = calls[1].PID
				if err := syscall.Kill(compactPID, 0); err != nil {
					t.Fatal("compact exited before Host shutdown", err)
				}
			}
			closed := make(chan error, 1)
			go func() { closed <- h.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Host shutdown waited for the 30-second compact instead of canceling it")
			}
			idleCompactions.Lock()
			_, remains := idleCompactions.entries[key]
			idleCompactions.Unlock()
			conversationLocks.Lock()
			_, locked := conversationLocks.entries[key]
			conversationLocks.Unlock()
			if remains || locked {
				t.Fatal("Host returned with a worker/timer/conversation lock still registered", remains, locked)
			}
			if compactPID != 0 {
				if err := syscall.Kill(compactPID, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatal("compact process was not reaped", err)
				}
				compactPID = 0
				view, err := s.Inspect(0, 256)
				if err != nil {
					t.Fatal(err)
				}
				interrupted := false
				for _, item := range view.History.Items {
					if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Context != nil && r.Context.Conversation != nil {
						c := r.Context.Conversation
						if c.Phase == "compact" && c.Interrupted {
							interrupted = true
						}
					}
				}
				if !interrupted {
					t.Fatal("Host closed before the compact cancellation record was saved")
				}
			} else {
				for _, call := range fake.Calls(t) {
					if strings.HasPrefix(call.Input, "/compact") {
						t.Fatal("pending timer ran during shutdown")
					}
				}
			}
		})
	}
}
