package contextbuilder

import (
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
)

func (b *builder) excerptResult(u contextengine.Unit, result llm.ToolResult, chars int) llm.ToolResult {
	info := b.receiptInfo[u.ID]
	streams := info.streams
	if u.Source.Sequence < b.forkSequence {
		// Inherited operations have evidence in the parent log.
		streams = nil
	}
	if b.blockedCalls[result.CallID] || b.windowSource.session == "" || u.Source.Sequence == 0 ||
		len(streams) == 0 && !info.directError {
		return result
	}
	preserveDisplay := chars == 0 && info.bounded
	if chars == 0 {
		chars = b.outputLimits[result.CallID]
		if chars <= 0 {
			chars = operation.DefaultMaxOutputLength
		}
	}
	if !info.preview {
		if preserveDisplay {
			return result
		}
		large := false
		for _, part := range result.Output {
			if part.Kind == llm.ToolResultText && len(part.Value) > chars && utf8.RuneCountInString(part.Value) > chars {
				large = true
				break
			}
		}
		if !large {
			return result
		}
	}

	var sources []string
	session := string(b.windowSource.session)
	for _, address := range streams {
		address.Session = session
		sources = append(sources, string(address.Locator()))
	}
	if info.directError {
		sources = append(sources, string(record.Address{
			Kind: record.ToolError, Session: session, Sequence: uint64(u.Source.Sequence),
		}.Locator()))
	}
	// Keep typed images unchanged. Excerpts are derived from the already
	// guarded text, so a cut cannot expose fragments of a redacted value.
	output := make([]llm.ToolResultOutput, 0, len(result.Output)+1)
	for _, part := range result.Output {
		if part.Kind == llm.ToolResultText && !preserveDisplay {
			part.Value, _ = operation.BoundOutput(part.Value, chars)
		}
		output = append(output, part)
	}
	result.Output = append(output, llm.ToolResultOutput{
		Kind: llm.ToolResultText, Value: "[Full tool output via record show: " + strings.Join(sources, ", ") + "]",
	})
	return result
}
