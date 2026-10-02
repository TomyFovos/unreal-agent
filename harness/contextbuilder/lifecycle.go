package contextbuilder

import (
	"fmt"
	"strings"
)

const oneShotEnding = "Ending a turn with no tool calls while calls are running means you sleep until one finishes; ending a turn with nothing running ends the session, so do that only when the task is complete."

// SetLifecycle changes mode guidance independently of provider/model profiles.
// The runtime, not the model or the profile, still owns termination.
func (current *builder) SetLifecycle(mode string) error {
	var ending string
	switch mode {
	case "one-shot":
		return nil
	case "interactive":
		ending = "Ending a turn means waiting for new inbox input or operation events. The host keeps the session alive until an explicit stop."
	case "child":
		ending = "Ending a turn means waiting for parent inbox input or operation events, including after you ask a question. Complete delegated work only with the explicit Finish tool, after all operations have finished."
	default:
		return fmt.Errorf("unknown lifecycle mode %q", mode)
	}
	current.preamble = strings.ReplaceAll(current.preamble, oneShotEnding, ending)
	current.preamble = strings.ReplaceAll(current.preamble, "As a backup, if calls are active and nothing has happened for ten minutes, a heartbeat wakes you, and this is an opportunity to check that all is well.", "The host delivers meaningful input and operation events; do not poll for status.")
	current.SetSystemPrompt(current.systemPrompt)
	return nil
}
