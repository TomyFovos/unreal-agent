package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestRespondLeavesThresholdCompactionToCLI(t *testing.T) {
	for _, input := range []int64{199_951, 399_951, 999_951} {
		t.Run(fmt.Sprint(input), func(t *testing.T) {
			fake := testclaude.New(t)
			fake.Set(t, testclaude.Config{AgentInputTokens: input})
			client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()})
			if err != nil {
				t.Fatal(err)
			}
			opt := llm.RequestOptions{Identity: llm.RequestIdentity{SessionID: "auto-compact"}, Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil }}
			first, err := client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("first")}}, opt)
			if err != nil {
				t.Fatal(err)
			}
			client.Idle(t.Context(), llm.Request{}, opt)
			input := append([]llm.Item{user("first")}, first.Output...)
			input = append(input, user("next child report"))
			if _, err := client.Respond(t.Context(), llm.Request{Input: input}, opt); err != nil {
				t.Fatal(err)
			}
			calls := fake.Calls(t)
			if len(calls) != 2 || calls[1].Input != "next child report" {
				t.Fatal(calls)
			}
			for _, call := range calls {
				args := strings.Join(call.Arguments, " ")
				if !strings.Contains(args, "--autocompact 233000") || strings.HasPrefix(call.Input, "/compact") {
					t.Fatal(call)
				}
			}
		})
	}
}

func TestAutoCompactArgumentRange(t *testing.T) {
	for _, tc := range []struct{ threshold, want int64 }{
		{200_000, 233_000}, {1, 100_000}, {67_000, 100_000}, {100_000, 133_000},
		{967_000, 1_000_000}, {1_000_000, 1_000_000}, {math.MaxInt64, 1_000_000},
	} {
		if got := autoCompactTokens(tc.threshold); got != tc.want {
			t.Errorf("threshold %d: got %d, want %d", tc.threshold, got, tc.want)
		}
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
			opt := llm.RequestOptions{
				Identity: llm.RequestIdentity{SessionID: "resume"}, ConversationTokens: map[string]int64{ProviderID: 500_000},
				Tools:              func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil },
				RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
			}
			client.compact(t.Context(), llm.Request{}, opt, sessionUUID("", "resume"), 500_000)
			_, err = client.Respond(t.Context(), llm.Request{Input: []llm.Item{assistant("earlier", true), user("continue the task")}}, opt)
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

func TestCompactPostTokensAreSeparateFromActualInput(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint("unknown=", unknown), func(t *testing.T) {
			fake := testclaude.New(t)
			fake.Set(t, testclaude.Config{CompactUnknownAfter: unknown})
			client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()})
			if err != nil {
				t.Fatal(err)
			}
			var records []llm.ConversationRecord
			opt := llm.RequestOptions{
				ConversationTokens: map[string]int64{ProviderID: 500_000},
				Tools:              func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil },
				RecordConversation: func(_ context.Context, r llm.ConversationRecord) error { records = append(records, r); return nil },
			}
			client.compact(t.Context(), llm.Request{}, opt, sessionUUID("", "unknown-post"), 500_000)
			_, err = client.Respond(t.Context(), llm.Request{Input: []llm.Item{assistant("earlier", true), user("next")}}, opt)
			if err != nil {
				t.Fatal(err)
			}
			wantAfter := int64(12_000)
			if unknown {
				wantAfter = 0
			}
			if len(records) != 2 || !records[0].Compacted || records[0].AfterTokens != wantAfter || records[0].InputKnown || records[0].InputTokens != 0 || !records[1].InputKnown || records[1].InputTokens != 60 {
				t.Fatalf("context observations = %+v", records)
			}
		})
	}
}

func TestCompactAggregateUsageDoesNotBecomePostTokens(t *testing.T) {
	state := &compaction{}
	state.handle(frame{Type: "result", Usage: &usage{InputTokens: 300_000}})
	if state.after != nil || state.usage == nil || state.usage.InputTokens != 300_000 {
		t.Fatal(state)
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
