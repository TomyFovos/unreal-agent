package claudeagent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// subagentLinePrefix indents a Claude Code Agent sub-agent's tool calls, so
// they sit under Claude's own "› scope" lines instead of reading as its own.
const subagentLinePrefix = "  ↳ "

// agentInput is the Agent tool's input: the task it was given and the model it
// runs on. approvalSubject's fields carry neither of them.
type agentInput struct {
	Description string `json:"description"`
	Model       string `json:"model"`
}

// agentCallSubject is the progress subject for an Agent call: the model and
// the task description, not the whole JSON input. chosen names the model
// picked with /model-sub, used when the call itself names none.
func agentCallSubject(input jsontext.Value, chosen func() string) string {
	var in agentInput
	_ = json.Unmarshal(input, &in)
	model := in.Model
	if model == "" && chosen != nil {
		model = chosen()
	}
	subject := "Claude Code Agent"
	if model != "" {
		subject += " · " + model
	}
	if in.Description != "" {
		subject += ": " + in.Description
	}
	return clip(subject, 512)
}

// agentModel is the model Claude's own Agent sub-agents run on, or "" when the
// choice is unset.
func (s SubagentCLI) agentModel() string {
	if s.AgentModel == nil {
		return ""
	}
	return s.AgentModel()
}
