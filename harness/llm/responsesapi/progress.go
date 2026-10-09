package responsesapi

import (
	"context"
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type progressKey struct{}
type attemptKey struct{}

func emitProgress(ctx context.Context, p llm.Progress) {
	if f, _ := ctx.Value(progressKey{}).(func(llm.Progress)); f != nil && ctx.Err() == nil {
		f(p)
	}
}
func emitTextProgress(ctx context.Context, payload []byte) {
	if f, _ := ctx.Value(progressKey{}).(func(llm.Progress)); f == nil {
		return
	}
	var e struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response struct {
			Usage *struct {
				Input  *int64 `json:"input_tokens"`
				Output *int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &e) != nil {
		return
	}
	if e.Type == "response.created" || e.Type == "response.in_progress" || e.Type == "response.completed" || e.Type == "response.incomplete" {
		if usage := e.Response.Usage; usage != nil {
			tokens := llm.TokenCounts{}
			if usage.Input != nil && *usage.Input >= 0 {
				tokens.Input, tokens.InputKnown = *usage.Input, true
			}
			if usage.Output != nil && *usage.Output >= 0 {
				tokens.Output, tokens.OutputKnown = *usage.Output, true
			}
			attempt, _ := ctx.Value(attemptKey{}).(uint64)
			emitProgress(ctx, llm.Progress{Attempt: attempt, Tokens: &tokens})
		}
	}
	if e.Type == "response.output_text.delta" {
		attempt, _ := ctx.Value(attemptKey{}).(uint64)
		// Bound the callback even if a provider emits an enormous SSE frame.
		if len(e.Delta) > 64<<10 {
			e.Delta = e.Delta[:64<<10]
		}
		emitProgress(ctx, llm.Progress{Attempt: attempt, Delta: e.Delta})
	}
}
