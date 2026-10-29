package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestCompactBeforeNextTurn(t *testing.T) {
	for _, tc := range []struct {
		name             string
		input, threshold int64
		compact          bool
	}{
		{"above_idle_threshold", 199_951, 0, false},
		{"below_twice", 399_949, 0, false},
		{"equal_twice", 399_950, 0, false},
		{"above_twice_including_caches", 399_951, 0, true},
		{"configured_threshold", 249, 149, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := testclaude.New(t)
			fake.Set(t, testclaude.Config{AgentInputTokens: tc.input})
			client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults(), CompactThresholdTokens: tc.threshold})
			if err != nil {
				t.Fatal(err)
			}
			var records []llm.ConversationRecord
			var phases []string
			options := llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "compact-session"},
				Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
					t.Fatal("compact must not publish a tool batch")
					return nil, nil
				},
				Progress: func(p llm.Progress) {
					phases = append(phases, p.Phase)
					if strings.Contains(p.Delta, "summary") {
						t.Fatal("compact summary leaked into progress")
					}
				},
				RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
			}
			first, err := client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("first task")}}, options)
			if err != nil {
				t.Fatal(err)
			}
			if len(fake.Calls(t)) != 1 {
				t.Fatal("compacted inside the first turn")
			}
			if len(records) != 1 || !records[0].InputKnown || records[0].InputTokens != tc.input+50 {
				t.Fatalf("reported context = %+v", records)
			}
			fake.Set(t, testclaude.Config{CompactPreTokens: tc.input + 50, CompactPostTokens: 12_345})
			input := append([]llm.Item{user("first task")}, first.Output...)
			input = append(input, user("Child worker-A ended; review its result."))
			reply, err := client.Respond(t.Context(), llm.Request{Input: input}, options)
			if err != nil {
				t.Fatal(err)
			}
			calls := fake.Calls(t)
			want := 2
			if tc.compact {
				want++
			}
			if len(calls) != want {
				t.Fatalf("calls = %+v", calls)
			}
			if calls[len(calls)-1].Input != "Child worker-A ended; review its result." {
				t.Fatalf("original input = %q", calls[len(calls)-1].Input)
			}
			if tc.compact {
				if calls[1].Input != compactPrompt {
					t.Fatalf("first input of next turn = %q", calls[1].Input)
				}
				for _, call := range calls[1:] {
					args := strings.Join(call.Arguments, " ")
					if !strings.Contains(args, "--resume "+sessionUUID("", "compact-session")) {
						t.Fatal("compact/turn changed conversation", args)
					}
				}
				if len(records) != 3 || !records[1].Compacted || records[1].BeforeTokens != tc.input+50 || !records[1].InputKnown || records[1].InputTokens != 12_345 {
					t.Fatalf("compact record = %+v", records)
				}
				if len(phases) < 2 || phases[0] != "compacting" || phases[1] != "" {
					t.Fatal("compact pulse not cleared", phases)
				}
			} else if len(phases) != 0 {
				t.Fatal("compact pulse below threshold", phases)
			}
			if len(reply.Output) != 1 || reply.Output[0].Data.(llm.Message).Text != "synthetic agent done" {
				t.Fatalf("canonical response contains compact output: %+v", reply.Output)
			}
		})
	}
}

func TestIdleCompactsOnlyAboveThreshold(t *testing.T) {
	for _, tc := range []struct {
		name             string
		input, threshold int64
		compact          bool
	}{
		{"below", 199_949, 0, false},
		{"equal", 199_950, 0, false},
		{"above_including_caches", 199_951, 0, true},
		{"configured_threshold", 100, 149, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := testclaude.New(t)
			fake.Set(t, testclaude.Config{AgentInputTokens: tc.input, CompactPreTokens: tc.input + 50, CompactPostTokens: 12_345})
			client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults(), CompactThresholdTokens: tc.threshold})
			if err != nil {
				t.Fatal(err)
			}
			var records []llm.ConversationRecord
			var phases []llm.Progress
			options := llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "idle-compact"},
				Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
					t.Error("maintenance published a tool batch")
					return nil, nil
				},
				Progress:           func(p llm.Progress) { phases = append(phases, p) },
				RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
			}
			request := llm.Request{Input: []llm.Item{user("first task")}}
			if _, err := client.Respond(t.Context(), request, options); err != nil {
				t.Fatal(err)
			}
			if calls := fake.Calls(t); len(calls) != 1 {
				t.Fatal("maintenance started before Respond returned", calls)
			}
			client.Idle(t.Context(), request, options)
			calls := fake.Calls(t)
			if !tc.compact {
				if len(calls) != 1 || len(records) != 1 || len(phases) != 0 {
					t.Fatalf("maintenance below threshold: calls=%+v records=%+v phases=%+v", calls, records, phases)
				}
				return
			}
			if len(calls) != 2 || calls[1].Input != compactPrompt || calls[1].OverlappingPID != 0 || !strings.Contains(strings.Join(calls[1].Arguments, " "), "--resume "+sessionUUID("", "idle-compact")) {
				t.Fatalf("idle compact process = %+v", calls)
			}
			if len(records) != 2 || !records[1].Compacted || records[1].BeforeTokens != tc.input+50 || !records[1].InputKnown || records[1].InputTokens != 12_345 {
				t.Fatalf("idle compact record = %+v", records)
			}
			if len(phases) != 2 || phases[0].Phase != "compacting" || !phases[1].Reset || phases[0].Delta != "" || phases[1].Delta != "" {
				t.Fatalf("idle compact progress = %+v", phases)
			}
		})
	}
}

func TestConversationLockAcrossClientsAndCanceledWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := &Client{config: Config{Directory: "/same-workspace"}}
		second := &Client{config: first.config}
		unlock, err := first.lockConversation(t.Context(), "same-conversation")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		waiter := make(chan error, 1)
		go func() {
			release, err := second.lockConversation(ctx, "same-conversation")
			if err == nil {
				release()
			}
			waiter <- err
		}()
		synctest.Wait()
		select {
		case err := <-waiter:
			t.Fatalf("another Client entered an owned conversation: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-waiter; !errors.Is(err, context.Canceled) {
			t.Fatal("lock wait ignored cancellation", err)
		}
		// Canceling a waiter must not remove the first Client's ownership.
		go func() {
			release, err := second.lockConversation(t.Context(), "same-conversation")
			if err == nil {
				release()
			}
			waiter <- err
		}()
		synctest.Wait()
		select {
		case err := <-waiter:
			t.Fatalf("canceled waiter lost the active process lock: %v", err)
		default:
		}
		other, err := second.lockConversation(t.Context(), "other-conversation")
		if err != nil {
			t.Fatal(err)
		}
		other()
		unlock()
		synctest.Wait()
		if err := <-waiter; err != nil {
			t.Fatal("next process did not acquire the released conversation", err)
		}
	})
}

func TestCompactFailureStillSendsOriginalInput(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "result_error", true: "process_exit"}[crash], func(t *testing.T) {
			fake := testclaude.New(t)
			fake.Set(t, testclaude.Config{CompactFailure: "synthetic compact rejected", CompactCrash: crash})
			client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()})
			if err != nil {
				t.Fatal(err)
			}
			var records []llm.ConversationRecord
			_, err = client.Respond(t.Context(), llm.Request{Input: []llm.Item{assistant("earlier", true), user("continue the task")}}, llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "resume"}, ConversationTokens: map[string]int64{ProviderID: 500_000},
				Tools:              func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil },
				RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := fake.Calls(t)
			if len(calls) != 2 || calls[0].Input != compactPrompt || calls[1].Input != "continue the task" {
				t.Fatalf("calls = %+v", calls)
			}
			if len(records) != 2 || records[0].Error == "" || records[0].Compacted || records[0].InputTokens != 500_000 {
				t.Fatalf("failed compact record = %+v", records)
			}
		})
	}
}

func TestCompactNeverAtToolBoundary(t *testing.T) {
	fake := testclaude.New(t)
	fake.Set(t, testclaude.Config{AgentInputTokens: 300_000, AgentStepInputTokens: 250_000,
		AgentSteps: []testclaude.AgentStep{{Tool: "Read", Input: jsontext.Value(`{"file_path":"/dev/null"}`)}, {Tool: "Read", Input: jsontext.Value(`{"file_path":"/dev/null"}`)}}})
	h := &harness{t: t, results: observed.NewResults(), workspace: t.TempDir()}
	var err error
	h.client, err = NewClient(Config{Binary: fake.Binary, Directory: h.workspace, Results: h.results})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.respond(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(t); len(calls) != 1 || strings.HasPrefix(calls[0].Input, "/compact") {
		t.Fatalf("compacted during tool work: %+v", calls)
	}
	if len(h.responses) != 2 {
		t.Fatal("did not exercise two tool boundaries", len(h.responses))
	}
}

func TestCompactUnknownPostTokens(t *testing.T) {
	fake := testclaude.New(t)
	fake.Set(t, testclaude.Config{CompactUnknownAfter: true})
	client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()})
	if err != nil {
		t.Fatal(err)
	}
	var records []llm.ConversationRecord
	_, err = client.Respond(t.Context(), llm.Request{Input: []llm.Item{assistant("earlier", true), user("next")}}, llm.RequestOptions{
		ConversationTokens: map[string]int64{ProviderID: 500_000},
		Tools:              func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil },
		RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !records[0].Compacted || records[0].InputKnown || !records[1].InputKnown || records[1].InputTokens != 60 {
		t.Fatalf("context observations = %+v", records)
	}
}

// Claude Code's result usage sums every model call of the turn. The context
// is the last call's input; a 7M-token sum must not trigger compaction.
func TestContextSizeIsTheLastCallNotTheTurnSum(t *testing.T) {
	fake := testclaude.New(t)
	fake.Set(t, testclaude.Config{AgentInputTokens: 7_000_000, AgentStepInputTokens: 50_000,
		AgentSteps: []testclaude.AgentStep{{Tool: "Read", Input: jsontext.Value(`{"file_path":"/dev/null"}`)}, {Tool: "Read", Input: jsontext.Value(`{"file_path":"/dev/null"}`)}}})
	h := &harness{t: t, results: observed.NewResults(), workspace: t.TempDir()}
	var err error
	h.client, err = NewClient(Config{Binary: fake.Binary, Directory: h.workspace, Results: h.results})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.respond(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.client.lastContextTokens(sessionUUID("", "session-1"), llm.RequestOptions{}); got != 50_000 {
		if got2 := h.client.lastContextTokens("session-1", llm.RequestOptions{}); got2 != 50_000 {
			t.Fatalf("recorded context = %d / %d, want the last call's 50000", got, got2)
		}
	}
}
