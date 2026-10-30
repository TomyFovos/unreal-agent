package claudeagent

import (
	"context"
	"log/slog"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func (t *turn) recordActivity(record llm.ActivityRecord) {
	if t.opt.RecordActivity != nil {
		if err := t.opt.RecordActivity(context.WithoutCancel(t.ctx), record); err != nil {
			slog.Warn("record Claude Code activity", "error", err)
		}
	}
}

// Result usage covers the whole process, including steps already recorded at
// tool boundaries. Keep it as an observation so reasoning reported only here
// is available without counting input/output twice.
func (t *turn) recordTurnUsage(result *frame) {
	if result.Usage == nil {
		return
	}
	u := reportedUsage(*result.Usage)
	t.recordActivity(llm.ActivityRecord{Kind: "claude_usage", ID: t.opt.Identity.TurnID, Provider: ProviderID, EndedAt: t.now().UTC(), Usage: &u})
}

func (t *turn) startNativeBash(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	if _, exists := t.toolStarts[id]; exists {
		t.mu.Unlock()
		return
	}
	started := t.now().UTC()
	t.toolStarts[id] = started
	t.mu.Unlock()
	// Claude Code exposes PreToolUse and tool_result, not its OS process clock.
	t.recordActivity(llm.ActivityRecord{Kind: "native_bash", ID: id, Tool: "Bash", StartedAt: started, Reason: "pre_tool_use"})
}

func (t *turn) endNativeBash(id, reason string) {
	t.mu.Lock()
	started, exists := t.toolStarts[id]
	delete(t.toolStarts, id)
	ended := t.now().UTC()
	t.mu.Unlock()
	if exists {
		t.recordActivity(llm.ActivityRecord{Kind: "native_bash", ID: id, Tool: "Bash", StartedAt: started, EndedAt: ended, Reason: reason})
	}
}

func (t *turn) endNativeBashCalls() {
	t.mu.Lock()
	var ids []string
	for id := range t.toolStarts {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	reason := "result_unavailable"
	if t.ctx.Err() != nil {
		reason = context.Cause(t.ctx).Error()
	}
	for _, id := range ids {
		t.endNativeBash(id, reason)
	}
}
