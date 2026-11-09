package claudeagent

import (
	"context"
	"log/slog"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// compactEvents observes automatic compaction inside a normal turn. It never
// treats the turn's aggregate usage as the context after a compact boundary.
type compactEvents struct {
	client        *Client
	turn          *turn
	started       time.Time
	succeeded     bool
	awaitingInput bool
	before        int64
	beforeKnown   bool
}

func (c *compactEvents) start() {
	if !c.started.IsZero() {
		return
	}
	c.started = c.turn.now().UTC()
	c.turn.mu.Lock()
	c.before, c.beforeKnown = c.turn.steps.contextTokens, c.turn.steps.contextKnown
	c.turn.mu.Unlock()
	if !c.beforeKnown {
		c.before = c.client.lastContextTokens(c.turn.session, c.turn.opt)
		c.beforeKnown = c.before > 0
	}
	c.turn.recordActivity(llm.ActivityRecord{Kind: "claude_compact", ID: c.started.Format(time.RFC3339Nano), Provider: ProviderID, StartedAt: c.started})
	if c.turn.opt.Progress != nil {
		c.turn.opt.Progress(llm.Progress{Attempt: 1, Phase: "compacting"})
	}
}

// handle returns true when the previous model input is no longer current.
func (c *compactEvents) handle(f frame) bool {
	if f.Type != "system" {
		return false
	}
	if f.Subtype == "status" {
		if f.Status == "compacting" {
			c.start()
		}
		switch f.CompactResult {
		case "success":
			c.start()
			c.succeeded = true
		case "failed":
			c.start()
			reason := clip(f.CompactError, 2000)
			if reason == "" {
				reason = "compaction failed"
			}
			c.record(llm.ConversationRecord{Error: reason, BeforeTokens: c.before, InputTokens: c.before, InputKnown: c.beforeKnown})
			slog.Warn("Claude Code automatic compaction failed", "session", c.turn.opt.Identity.SessionID, "error", reason)
		}
	}
	if f.Subtype != "compact_boundary" || f.CompactMetadata == nil {
		return false
	}
	c.start()
	meta := f.CompactMetadata
	ended := c.turn.now().UTC()
	record := llm.ConversationRecord{BeforeTokens: meta.PreTokens, Compacted: true, EndedAt: ended, StartedAt: c.started}
	if meta.DurationMS > 0 {
		record.StartedAt = ended.Add(-time.Duration(meta.DurationMS) * time.Millisecond)
	}
	if meta.PostTokens != nil {
		record.AfterTokens = *meta.PostTokens
	}
	c.record(record)
	c.awaitingInput = true
	return true
}

func (c *compactEvents) record(record llm.ConversationRecord) {
	record.Provider, record.Phase = ProviderID, "compact"
	if record.StartedAt.IsZero() {
		record.StartedAt = c.started
	}
	if record.EndedAt.IsZero() {
		record.EndedAt = c.turn.now().UTC()
	}
	c.client.recordConversation(context.WithoutCancel(c.turn.ctx), c.turn.opt, c.turn.session, record)
	c.turn.recordActivity(llm.ActivityRecord{Kind: "claude_compact", ID: c.started.Format(time.RFC3339Nano), Provider: ProviderID, StartedAt: record.StartedAt, EndedAt: record.EndedAt, Reason: record.Error})
	if c.turn.opt.Progress != nil {
		c.turn.opt.Progress(llm.Progress{Attempt: 1, Reset: true})
	}
	c.started, c.succeeded = time.Time{}, false
}

func (c *compactEvents) finish() {
	if c.started.IsZero() {
		return
	}
	record := llm.ConversationRecord{Compacted: c.succeeded}
	if c.turn.ctx.Err() != nil {
		record.Interrupted = true
		record.InterruptReason = context.Cause(c.turn.ctx).Error()
	} else if !c.succeeded {
		record.Error = "compaction ended without a result"
	}
	c.record(record)
}
