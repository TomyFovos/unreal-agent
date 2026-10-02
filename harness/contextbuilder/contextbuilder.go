// Package contextbuilder defines I/O-pure, in-memory model request construction.
package contextbuilder

import (
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

type ChangeKind string

const (
	ChangeOmitted   ChangeKind = "omitted"
	ChangeTruncated ChangeKind = "truncated"
	ChangeCompacted ChangeKind = "compacted"
)

type Change struct {
	Kind   ChangeKind
	Source string
	Reason string
}

type Report struct {
	Changes []Change
}

type Result struct {
	Request llm.Request
	Report  Report
}

// Builder retains model request state without performing I/O.
type Builder interface {
	AddExternalInput(inbox.Input) error
	AddControlMessage(inbox.ControlMessage)
	SetModel(llm.Model)
	SetSystemPrompt(string)
	// SetProjectInstructions binds a session's project instruction snapshot,
	// kept separate from the system prompt and skills.
	SetProjectInstructions(projectinstructions.Snapshot) error
	AddModelResponse(llm.Response)
	AddReasoning(llm.Reasoning)
	AddTool(llm.Tool)
	AddToolResult(string, []llm.ToolResultOutput, bool)
	Commit()
	Build() (Result, error)
}
