package claudeagent

import (
	"encoding/json/v2"
)

// launchSettings is the single --settings value the turn needs: the exclusion
// list upstream computes, plus, when the Session has an agent-facing CLI, the Host's own
// socket. Claude Code's sandbox blocks that socket, so the subagent command it
// runs would fail with "gateway: disconnected"; --settings overrides only the
// keys it sets, and allowUnixSockets merges with the sockets the user's
// settings already allow, so the user's sandbox stays on. The per-launch env
// also takes precedence over env entries in personal and project settings.
func launchSettings(directory string, env []string, subagents SubagentCLI) string {
	upstream := ancestorInstructionSettings(directory, env)
	socket := sessionCommandAvailable(subagents)
	// Upstream's keys are its own: decode the object generically and add the
	// sandbox beside them.
	var settings map[string]any
	if upstream != "" {
		_ = json.Unmarshal([]byte(upstream), &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	if socket {
		settings["sandbox"] = map[string]any{"network": map[string]any{"allowUnixSockets": []string{subagents.Socket}}}
	}
	settings["env"] = map[string]string{disableAutoCompactEnv: "1", disableCompactEnv: "1"}
	data, _ := json.Marshal(settings)
	return string(data)
}
