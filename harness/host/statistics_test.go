package host

import (
	"context"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestGenerationClockExcludesApprovalAndToolWaitAndFreezes(t *testing.T) {
	s := progressSession()
	now := time.Unix(100, 0)
	var emit func(llm.Progress)
	next := modelFunc(func(ctx context.Context, _ llm.Request, o llm.RequestOptions) (llm.Response, error) {
		emit = o.Progress
		o.Progress(llm.Progress{Attempt: 1, Reset: true})
		now = now.Add(time.Second)
		input := llm.TokenCounts{Input: 120, InputKnown: true}
		o.Progress(llm.Progress{Attempt: 1, Tokens: &input})
		output := llm.TokenCounts{Output: 20, OutputKnown: true}
		o.Progress(llm.Progress{Attempt: 1, Tokens: &output})
		v, _ := s.Inspect(0, 1)
		if stats := v.Progress.Statistics; stats == nil || stats.Tokens.Input != 120 || stats.Tokens.Output != 20 || stats.DurationMicros != 1_000_000 {
			t.Fatal("partial reported counters not merged", stats)
		}
		if _, err := o.Tools(ctx, llm.Response{Usage: llm.Usage{InputTokens: 120, OutputTokens: 20}}); err != nil {
			return llm.Response{}, err
		}
		now = now.Add(2 * time.Second)
		return llm.Response{Usage: llm.Usage{InputTokens: 130, OutputTokens: 40}}, nil
	})
	a := &progressAdapter{session: s, next: next, now: func() time.Time { return now }}
	r, err := a.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "pinned-model"}}, llm.RequestOptions{Tools: func(_ context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		if rate, ok := response.Generation.OutputTPS(); !ok || rate != 20 {
			t.Fatal("tool request statistics", response.Generation)
		}
		before, _ := s.Inspect(0, 1)
		now = now.Add(time.Hour) // deterministic stand-in for detached approval + execution
		unexpected := llm.TokenCounts{Output: 999, OutputKnown: true}
		emit(llm.Progress{Attempt: 2, Reset: true, Tokens: &unexpected, Delta: "during execution"})
		after, _ := s.Inspect(0, 1)
		if *before.Progress.Statistics != *after.Progress.Statistics || after.Progress.Text != "" || !after.Progress.Done {
			t.Fatal("wait changed frozen inference statistics")
		}
		s.mu.Lock()
		s.rememberItem(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn1", Response: response}}, false)
		s.rememberItem(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn2", PreviousTurnID: "turn1", ToolContinuation: "turn1"}}, false)
		s.mu.Unlock()
		return nil, nil
	}})
	if err != nil || r.Generation == nil || r.Generation.Model != "pinned-model" || r.Generation.Source != "client_clock" || r.Generation.DurationMicros != 2_000_000 {
		t.Fatal("tool wait included in response duration", r.Generation, err)
	}
	if rate, ok := r.Generation.OutputTPS(); !ok || rate != 20 {
		t.Fatal(rate, ok)
	}
	view, _ := s.Inspect(0, 10)
	before := *view.Progress.Statistics
	emit(llm.Progress{Attempt: 99, Reset: true, Tokens: &llm.TokenCounts{Output: 999, OutputKnown: true}})
	view, _ = s.Inspect(0, 10)
	if *view.Progress.Statistics != before {
		t.Fatal("late callback replaced completed TPS")
	}
}

func TestGenerationProviderDurationOverridesClientAndUnknownStaysUnknown(t *testing.T) {
	for _, measured := range []bool{false, true} {
		s := progressSession()
		now := time.Unix(100, 0)
		a := &progressAdapter{session: s, now: func() time.Time { return now }, next: modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			now = now.Add(time.Hour)
			r := llm.Response{}
			if measured {
				r.Generation = &llm.GenerationStatistics{Source: "provider_api", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{Output: 8, OutputKnown: true}}
			}
			return r, nil
		})}
		r, err := a.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "selected"}}, llm.RequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		rate, known := r.Generation.OutputTPS()
		if known != measured || measured && (rate != 8 || r.Generation.Source != "provider_api") {
			t.Fatal(r.Generation)
		}
	}
}

func TestGenerationZeroClockDoesNotInventCanonicalTiming(t *testing.T) {
	s := progressSession()
	now := time.Unix(100, 0)
	response := llm.Response{Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}
	a := &progressAdapter{session: s, now: func() time.Time { return now }, next: modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) { return response, nil })}
	r, err := a.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	if err != nil || r.Generation != nil || r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 5 {
		t.Fatal("unmeasurable time changed provider response", r, err)
	}
}
