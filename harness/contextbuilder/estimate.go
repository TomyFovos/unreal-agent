package contextbuilder

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"math"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// A new epoch identifies a changed historical projection. It prevents using a
// successful request's usage after pruning, compaction or portable replay.
type estimateEpoch struct{ marker bool }

type requestEstimate struct {
	tokens int64
	header [32]byte
	epoch  *estimateEpoch
}

type derivedEstimate struct {
	through     string
	tokens      int64
	prefix      int64
	prefixValid bool
	header      [32]byte
	epoch       *estimateEpoch
}

func (b *builder) tokenEstimator() contextengine.TokenEstimator {
	return contextengine.TokenEstimator{Provider: b.provider}
}

// Item token contributions are additive. Cache them across appended Turns,
// including canonical replay; view changes invalidate this cache together with
// derived representations. Header edits invalidate only the prefix.
func (b *builder) estimateRequest() int64 {
	e := b.tokenEstimator()
	if b.estimate.epoch == nil {
		b.estimate.epoch = &estimateEpoch{}
	}
	for _, u := range b.engine.UnitsAfter(b.estimate.through) {
		b.estimate.through = u.ID
		if u.ID <= b.viewThrough && !b.retained[u.ID] || u.Class == contextengine.Omit {
			continue
		}
		b.estimate.tokens += e.Items(b.deriveUnit(u))
	}
	if !b.estimate.prefixValid {
		instructions, _ := contextengine.PublicItem(b.committedPrefix[0])
		input := append([]llm.Item{instructions}, b.summary...)
		b.estimate.prefix = e.Request(llm.Request{Tools: b.request.Tools, Input: input})
		data, err := json.Marshal(struct {
			Provider string
			Revision uint64
			Model    llm.Model
			Tools    []llm.Tool
			Input    []llm.Item
		}{b.provider, b.contextRuntime.Revision, b.request.Model, b.request.Tools, input}, json.Deterministic(true))
		if err != nil {
			// Do not match a measured prefix whose header cannot be encoded.
			b.estimate.epoch = &estimateEpoch{}
		}
		b.estimate.header = sha256.Sum256(data)
		b.estimate.prefixValid = true
	}
	if e.Claude() {
		return e.Request(llm.Request{Input: b.deriveItems()})
	}
	return b.estimate.prefix + b.estimate.tokens
}

func (b *builder) requestEstimate() requestEstimate {
	tokens := b.estimateRequest()
	return requestEstimate{tokens: tokens, header: b.estimate.header, epoch: b.estimate.epoch}
}

func (b *builder) admissionFingerprint(header [32]byte) string {
	if b.filteredOutput {
		// Older turns may have sent capped output even with the same header.
		header = sha256.Sum256(append(header[:], "exclude-output-limit"...))
	}
	return fmt.Sprintf("%x", header)
}

// inputTokens uses the last successful request's reported input only for the
// same provider/model/header and unchanged historical prefix. The new response
// and subsequent inputs/results are the estimated delta, never free input.
func (b *builder) inputTokens() (tokens, estimate, delta int64, measurement string) {
	current := b.requestEstimate()
	estimate = current.tokens
	previous := b.lastEstimate
	if b.lastInputKnown && previous.epoch != nil && previous.epoch == current.epoch && previous.header == current.header {
		delta = current.tokens - previous.tokens
		if delta > 0 && b.lastInput > math.MaxInt64-delta {
			return math.MaxInt64, estimate, delta, "measured-prefix+estimated-delta"
		}
		return max(0, b.lastInput+delta), estimate, delta, "measured-prefix+estimated-delta"
	}
	return estimate, estimate, 0, "estimated"
}
