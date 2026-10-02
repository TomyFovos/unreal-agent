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
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if json.Unmarshal(payload, &e) == nil && e.Type == "response.output_text.delta" {
		attempt, _ := ctx.Value(attemptKey{}).(uint64)
		// Bound the callback even if a provider emits an enormous SSE frame.
		if len(e.Delta) > 64<<10 {
			e.Delta = e.Delta[:64<<10]
		}
		emitProgress(ctx, llm.Progress{Attempt: attempt, Delta: e.Delta})
	}
}
