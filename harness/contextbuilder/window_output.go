package contextbuilder

import (
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Only small, public receipt metadata is retained here, never raw Operation state.
type receiptInfo struct {
	streams          []record.Address
	directError      bool
	preview, bounded bool
}

func toolReceiptInfo(status sessionstore.ToolCallStatus) receiptInfo {
	var info receiptInfo
	info.directError = status.Status.Error != "" && (!status.Status.ErrorTruncated || len(status.Status.OriginalError) > 0)
	info.preview = status.Status.ErrorTruncated
	for _, op := range status.Operations {
		for _, stream := range operation.RecordStreams(op) {
			info.streams = append(info.streams, record.Address{Kind: record.OperationOutput, Operation: string(op.ID), Stream: stream})
		}
		if op.Type == operation.TypeShell {
			if state, err := operation.DecodeShellState(op); err == nil {
				info.preview = info.preview || state.OutTruncated || state.ErrTruncated || state.ErrorTruncated
			}
		}
		if op.Type != operation.TypeRemoteJob {
			continue
		}
		state, err := operation.DecodeRemoteJobState(op)
		if err != nil {
			continue
		}
		info.preview = info.preview || state.ResultTruncated || state.ErrorTruncated
	}
	return info
}

func (b *builder) excerptResult(u contextengine.Unit, result llm.ToolResult, chars int, fit *int) llm.ToolResult {
	if b.blockedCalls[result.CallID] || b.windowSource.session == "" || u.Source.Sequence == 0 {
		return result
	}
	info := b.receiptInfo[u.ID]
	streams := info.streams
	if u.Source.Sequence < b.forkSequence {
		// Inherited operations have evidence in the parent log.
		streams = nil
	}
	hasOutput := len(streams) > 0 || info.directError
	if !hasOutput && fit == nil {
		return result
	}
	preserveDisplay := chars == 0 && info.bounded
	if chars == 0 {
		chars = b.outputLimits[result.CallID]
		if chars <= 0 {
			chars = operation.DefaultMaxOutputLength
		}
	}
	if fit != nil {
		if preserveDisplay || !hasOutput {
			chars = *fit
		} else {
			chars = min(chars, *fit)
		}
		preserveDisplay = false
	}

	// Excerpt only guarded text. Typed images and the receipt identity are
	// unchanged. Copy output parts only when the projection changes.
	output := result.Output
	excerpted := false
	if !preserveDisplay {
		for i, part := range result.Output {
			if part.Kind != llm.ToolResultText || len(part.Value) <= chars {
				continue
			}
			bounded, cut := operation.BoundOutput(part.Value, chars)
			if !cut || fit != nil && textWeight(bounded) >= textWeight(part.Value) {
				continue
			}
			if !excerpted {
				output = slices.Clone(result.Output)
				excerpted = true
			}
			output[i].Value = bounded
		}
	}
	if !excerpted && !(hasOutput && info.preview) {
		return result
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
	label := "Full tool output"
	if len(sources) == 0 {
		label = "Canonical tool receipt"
		sources = append(sources, string(record.Item(session, uint64(u.Source.Sequence))))
	}
	result.Output = append(slices.Clip(output), llm.ToolResultOutput{
		Kind: llm.ToolResultText, Value: "[" + label + " via record show: " + strings.Join(sources, ", ") + "]",
	})
	return result
}

func (b *builder) excerptMessage(u contextengine.Unit, message llm.Message, chars int) llm.Message {
	if b.windowSource.session == "" || u.Source.Sequence == 0 || len(message.Text) <= chars {
		return message
	}
	text, cut := operation.BoundOutput(message.Text, chars)
	if !cut {
		return message
	}
	a := record.Address{Session: string(b.windowSource.session), Sequence: uint64(u.Source.Sequence), Kind: record.SessionItem}
	switch u.Source.SourceType {
	case string(sessionstore.ItemInput):
		switch u.Kind {
		case contextengine.UserMessage:
			if u.Required || u.Class == contextengine.Pin {
				a.Kind = record.UserMessage
			}
		case contextengine.ChildResult:
			a.Kind = record.PeerMessage
		}
	case string(sessionstore.ItemModelResponse):
		if message.Role == llm.RoleAssistant {
			a.Kind, a.Part = record.AssistantMessage, u.Source.OutputIndex
		}
	}
	message.Text = text + "\n[Full message via record show: " + string(a.Locator()) + "]"
	return message
}

func textWeight(text string) int64 {
	size := contextengine.MeasureText(text)
	return size.ASCII + 4*size.Other
}
