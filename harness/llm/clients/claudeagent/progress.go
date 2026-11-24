package claudeagent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// agentInput is the Agent tool's input: the task it was given and the model it
// runs on. approvalSubject's fields carry neither of them.
type agentInput struct {
	Description string `json:"description"`
	Model       string `json:"model"`
}

// agentCallSubject formats legacy Agent calls and denied attempts.
func agentCallSubject(input jsontext.Value) string {
	var in agentInput
	_ = json.Unmarshal(input, &in)
	subject := "Claude Code Agent"
	if in.Model != "" {
		subject += " · " + in.Model
	}
	if in.Description != "" {
		subject += ": " + in.Description
	}
	return clip(subject, 512)
}
