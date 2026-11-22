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
// successful request's usage after a window rebuild or portable replay.
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
	window      windowPromptSize
	windowHead  windowPromptSize
	prefixValid bool
	header      [32]byte
	epoch       *estimateEpoch
}

// Window lines join into one prompt, so rounding any line separately would
// change the whole-prompt estimate. Measure each line once and round the sum.
type windowPromptSize struct {
	text  contextengine.TextSize
	lines int64
}

func (s *windowPromptSize) add(other windowPromptSize) {
	s.text.ASCII += other.text.ASCII
	s.text.Other += other.text.Other
	s.lines += other.lines
}

func (s windowPromptSize) tokens() int64 {
	if s.lines == 0 {
		return contextengine.MeasureText(llm.EmptyWindowPrompt).Tokens()
	}
	s.text.ASCII += (s.lines - 1) * int64(len(llm.WindowPromptSeparator))
	return s.text.Tokens()
}

func (b *builder) measureWindowPrompt(items []llm.Item) windowPromptSize {
	var size windowPromptSize
	for _, item := range items {
		if line, ok := b.windowPrompt(item); ok {
			size.add(windowPromptSize{text: contextengine.MeasureText(line), lines: 1})
		}
	}
	return size
}

func (b *builder) tokenEstimator() contextengine.TokenEstimator {
	return contextengine.TokenEstimator{Provider: b.provider}
}

// Item token contributions are additive. Cache them across appended Turns,
// including canonical replay; view changes invalidate this cache together with
// derived representations. Header edits invalidate only the prefix.
func (b *builder) estimateRequest() int64 {
	e := b.tokenEstimator()
	windowPrompt := b.windowPrompt != nil
	if b.estimate.epoch == nil {
		b.estimate.epoch = &estimateEpoch{}
	}
	for _, u := range b.projectionUnitsAfter(b.estimate.through) {
		b.estimate.through = u.ID
		if !b.includesUnit(u) {
			continue
		}
		items := b.deriveUnit(u)
		if windowPrompt {
			b.estimate.window.add(b.measureWindowPrompt(items))
		} else {
			b.estimate.tokens += e.Items(items)
		}
	}
	if !b.estimate.prefixValid {
		input := b.contextPrefix()
		if windowPrompt {
			b.estimate.windowHead = b.measureWindowPrompt(input)
			b.estimate.prefix = b.windowHeaderTokens(input[0])
		} else {
			b.estimate.prefix = b.windowHeaderTokens(input[0]) + e.Items(input[1:])
		}
		data, err := json.Marshal(struct {
			Provider      string
			Revision      uint64
			Model         llm.Model
			Tools         []llm.Tool
			Input         []llm.Item
			PublicHistory bool `json:",omitzero"`
		}{b.provider, b.contextRuntime.Revision, b.request.Model, b.request.Tools, input, true}, json.Deterministic(true))
		if err != nil {
			// Do not match a measured prefix whose header cannot be encoded.
			b.estimate.epoch = &estimateEpoch{}
		}
		b.estimate.header = sha256.Sum256(data)
		b.estimate.prefixValid = true
	}
	if windowPrompt {
		size := b.estimate.windowHead
		size.add(b.estimate.window)
		return b.estimate.prefix + size.tokens()
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
	if b.filteredScopes {
		header = sha256.Sum256(append(header[:], "exclude-nested-tools"...))
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
