package contextbuilder

import (
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type derivedEstimate struct {
	through     string
	bytes       int64
	invalid     bool
	prefix      int64
	prefixValid bool
}

// JSON array lengths are additive: each item after the mandatory instruction
// contributes its encoded bytes and one comma. Cache those contributions while
// the derived view is unchanged, rather than serializing the entire growing
// conversation at every restored Turn. Explicit projection/runtime changes
// invalidate this cache alongside the existing derived representations.
func (b *builder) estimateRequest() int64 {
	for _, u := range b.engine.UnitsAfter(b.estimate.through) {
		b.estimate.through = u.ID
		if u.ID <= b.viewThrough && !b.retained[u.ID] || u.Class == contextengine.Omit {
			continue
		}
		for _, item := range b.deriveUnit(u) {
			// Keep the Request.Input nesting depth. Encoding an item alone
			// could accept opaque JSON that the complete request rejects.
			encoded, err := json.Marshal(struct{ Input []llm.Item }{[]llm.Item{item}})
			if err != nil {
				b.estimate.invalid = true
				continue
			}
			b.estimate.bytes += int64(len(encoded)-len(`{"Input":[]}`)) + 1
		}
	}
	if b.estimate.invalid {
		return 1 << 60
	}
	if !b.estimate.prefixValid {
		instructions, _ := contextengine.PublicItem(b.committedPrefix[0])
		input := append([]llm.Item{instructions}, b.summary...)
		b.estimate.prefix = contextengine.Estimate(llm.Request{Model: b.request.Model, Tools: b.request.Tools, Input: input})
		b.estimate.prefixValid = true
	}
	if b.estimate.prefix == 1<<60 {
		return b.estimate.prefix
	}
	return b.estimate.prefix + b.estimate.bytes
}
