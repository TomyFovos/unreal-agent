package coordinator

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func (current *coordinator) recordContextRedactions(ctx context.Context) error {
	if b, ok := current.dependencies.ContextBuilder.(interface {
		RecordRedactions(func(llm.ActivityRecord) error) error
	}); ok {
		return b.RecordRedactions(func(record llm.ActivityRecord) error {
			if current.dependencies.RecordActivity != nil {
				return current.dependencies.RecordActivity(ctx, record)
			}
			return nil
		})
	}
	return nil
}
