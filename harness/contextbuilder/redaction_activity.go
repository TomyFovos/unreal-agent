package contextbuilder

import (
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func (b *builder) queueRedaction(id string, r *llm.ContextRedaction) {
	if r != nil {
		b.redactions = append(b.redactions, llm.ActivityRecord{Kind: "context_redaction", ID: id, Redaction: r})
	}
}

// RecordRedactions lets the owning Coordinator persist projections once. A
// failed append retains that measurement and the remaining queue for retry.
func (b *builder) RecordRedactions(record func(llm.ActivityRecord) error) error {
	for len(b.redactions) > 0 {
		activity := b.redactions[0]
		activity.StartedAt = time.Now().UTC()
		if err := record(activity); err != nil {
			return err
		}
		b.redactions[0] = llm.ActivityRecord{}
		b.redactions = b.redactions[1:]
	}
	return nil
}
