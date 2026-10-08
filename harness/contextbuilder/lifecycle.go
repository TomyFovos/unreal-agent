package contextbuilder

import (
	"fmt"
	"strings"
)

const oneShotEnding = "Ending a turn with no tool calls while calls are running means you sleep until one finishes; ending a turn with nothing running ends the session, so do that only when the task is complete."
const textOnlyEnding = "Ending a turn ends the session, so do that only when the task is complete."

// SetLifecycle changes mode guidance independently of provider/model profiles.
// The runtime, not the model or the profile, still owns termination.
func (current *builder) SetLifecycle(mode string) error {
	switch mode {
	case "one-shot", "interactive", "child":
	default:
		return fmt.Errorf("unknown lifecycle mode %q", mode)
	}
	current.lifecycle = mode
	current.applyLifecycle()
	current.renderSystem()
	return nil
}

// Provider changes rebuild the harness preamble, but the Host's existing
// lifecycle is still authoritative. This is only derived request guidance.
func (current *builder) applyLifecycle() {
	var ending string
	switch current.lifecycle {
	case "", "one-shot":
		return
	case "interactive":
		ending = "Ending a turn means waiting for new inbox input or operation events. The host keeps the session alive until an explicit stop."
	case "child":
		ending = "Ending a turn means waiting for parent inbox input or operation events, including after you ask a question. Complete delegated work only with the explicit Finish tool, after all operations have finished."
		if current.textOnly {
			ending = "Ending your text response completes this delegated child. Unreal records its completion; no Finish tool is available."
		}
	}
	current.preamble = strings.ReplaceAll(current.preamble, oneShotEnding, ending)
	current.preamble = strings.ReplaceAll(current.preamble, textOnlyEnding, ending)
	current.preamble = strings.ReplaceAll(current.preamble, "As a backup, if calls are active and nothing has happened for ten minutes, a heartbeat wakes you, and this is an opportunity to check that all is well.", "The host delivers meaningful input and operation events; do not poll for status.")
}
