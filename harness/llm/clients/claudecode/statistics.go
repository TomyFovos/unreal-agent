package claudecode

import "github.com/unreallabsai/unreal-agent/harness/llm"

// Called only after the existing frame validation. Ignore text/IDs/reasoning and
// only expose explicit API token counters, never serializer input/proposals.
func emitTokenProgress(event wireEvent, callback func(llm.Progress)) {
	if callback == nil {
		return
	}
	var wire *wireUsage
	if event.Type == "message_start" {
		wire = event.Message.Usage
	}
	if event.Type == "message_delta" {
		wire = event.Usage
	}
	if wire == nil {
		return
	}
	usage, err := normalizeUsage(wire)
	if err != nil {
		return
	} // Diagnostics do not loosen the authoritative parser.
	tokens := llm.ReportedTokens(usage)
	callback(llm.Progress{Attempt: 1, Tokens: &tokens})
}
