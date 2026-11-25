package claudeagent

import (
	"encoding/json/v2"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// StreamCall identifies a model request in Claude Code's resumable execution.
// A canonical ModelResponse can summarize several of these requests.
func StreamCall(activity llm.ActivityRecord) (session.TurnID, bool) {
	if activity.Kind != "claude_stream" || activity.Model == "<synthetic>" {
		return "", false
	}
	var event struct {
		Event      string `json:"event"`
		Parent     string `json:"parent_tool_use_id"`
		Compaction bool   `json:"compaction"`
	}
	if json.Unmarshal([]byte(activity.Reason), &event) != nil ||
		event.Event != "message_start" || event.Parent != "" || event.Compaction {
		return "", false
	}
	turn, _, ok := strings.Cut(activity.ID, "/stream/")
	return session.TurnID(turn), ok
}
