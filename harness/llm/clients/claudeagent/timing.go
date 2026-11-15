package claudeagent

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type startupTiming struct {
	process, init, message, text time.Time
}

// Each interval is recorded once, so startup, provider latency and streaming
// latency can be distinguished without storing any message content.
func (t *turn) recordStartup(f frame) {
	if t.startup.process.IsZero() {
		return
	}
	now := t.now().UTC()
	record := func(reason string, started time.Time) {
		if !started.IsZero() {
			t.recordActivity(llm.ActivityRecord{Kind: "claude_startup", ID: t.opt.Identity.TurnID, Provider: ProviderID, Reason: reason, StartedAt: started, EndedAt: now})
		}
	}
	if f.Type == "system" && f.Subtype == "init" && t.startup.init.IsZero() {
		t.startup.init = now
		record("process_to_init", t.startup.process)
	}
	if f.ParentToolUseID != nil {
		return
	}
	text := false
	if f.Type == "stream_event" {
		var event streamEvent
		if json.Unmarshal(f.Event, &event) != nil {
			return
		}
		if event.Type == "message_start" && t.startup.message.IsZero() {
			t.startup.message = now
			record("init_to_message_start", t.startup.init)
		}
		text = event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != ""
	} else if f.Type == "assistant" {
		var m message
		if json.Unmarshal(f.Message, &m) == nil {
			for _, b := range m.Content {
				text = text || b.Type == "text" && b.Text != ""
			}
		}
	}
	if text && t.startup.text.IsZero() {
		t.startup.text = now
		record("message_start_to_text", t.startup.message)
	}
}

func (t *turn) recordActivity(record llm.ActivityRecord) {
	if t.opt.RecordActivity != nil {
		if err := t.opt.RecordActivity(context.WithoutCancel(t.ctx), record); err != nil {
			slog.Warn("record Claude Code activity", "error", err)
		}
	}
}

// result.usage is authoritative for this execution, including discarded output
// and recovery attempts. Assistant fragments can omit most of that output;
// result.modelUsage instead accumulates across resumed conversations.
func (t *turn) recordTurnUsage(result *frame) {
	totals := map[string]llm.Usage{}
	t.mu.Lock()
	model := t.model
	for _, m := range t.messages {
		model := m.Model
		if model == "" {
			model = t.model
		}
		if model != "<synthetic>" {
			totals[model] = addUsage(totals[model], reportedUsage(*m.Usage))
		}
	}
	t.mu.Unlock()
	if t.streams != nil {
		t.streams.mu.Lock()
		if t.streams.mainModel != "" {
			model = t.streams.mainModel
		}
		for name, u := range t.streams.usage {
			if name == "" {
				name = model
			}
			totals[name] = u
		}
		t.streams.mu.Unlock()
	}
	if model == "" && len(totals) == 1 {
		for name := range totals {
			model = name
		}
	}
	if result.Usage != nil {
		u := reportedUsage(*result.Usage)
		// Preserve native Agent model attribution, using complete streams when
		// available. The main model receives the remainder of the SDK total.
		main := u
		for name, child := range totals {
			if name != model {
				main.InputTokens -= child.InputTokens
				main.CachedInputTokens -= child.CachedInputTokens
				main.CacheWriteInputTokens -= child.CacheWriteInputTokens
				main.OutputTokens -= child.OutputTokens
				if !slices.Contains(main.Unknown, llm.UsageReasoning) {
					if slices.Contains(child.Unknown, llm.UsageReasoning) {
						main.Unknown = append(main.Unknown, llm.UsageReasoning)
						main.ReasoningTokens = 0
					} else {
						main.ReasoningTokens -= child.ReasoningTokens
					}
				}
			}
		}
		totals[model] = main
		// This is the same usage as provider_usage, at execution granularity.
		// Consumers replace same-turn rows with it, never add it to them.
		t.recordActivity(llm.ActivityRecord{Kind: "claude_usage", ID: t.opt.Identity.TurnID, Provider: ProviderID, Model: model, EndedAt: t.now().UTC(), Usage: &u})
	}
	for model, u := range totals {
		if model != "" && model != "<synthetic>" {
			t.recordActivity(llm.ActivityRecord{Kind: "provider_usage", ID: t.opt.Identity.TurnID, Provider: ProviderID, Model: model, EndedAt: t.now().UTC(), Usage: &u})
		}
	}
}

func addUsage(a, b llm.Usage) llm.Usage {
	a.InputTokens += b.InputTokens
	a.CachedInputTokens += b.CachedInputTokens
	a.CacheWriteInputTokens += b.CacheWriteInputTokens
	a.OutputTokens += b.OutputTokens
	a.ReasoningTokens += b.ReasoningTokens
	for _, field := range b.Unknown {
		if !slices.Contains(a.Unknown, field) {
			a.Unknown = append(a.Unknown, field)
		}
	}
	return a
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

type nativeToolActivity struct {
	tool    string
	started time.Time
}

// Non-Bash tools have no canonical receipt until PostToolBatch either. Record
// their public tool_use boundary so a live native tool (including Agent or a
// read/network tool without an approval hook) can be distinguished from a CLI
// waiting for its next model response. This interval includes approval waits;
// it is not the OS execution clock and never enters generation/TPS statistics.
func (t *turn) startNativeTool(id, name string) {
	if id == "" || name == "" {
		return
	}
	t.mu.Lock()
	if t.nativeTools == nil {
		t.nativeTools = map[string]nativeToolActivity{}
	}
	if _, exists := t.nativeTools[id]; exists {
		t.mu.Unlock()
		return
	}
	activity := nativeToolActivity{tool: name, started: t.now().UTC()}
	t.nativeTools[id] = activity
	t.mu.Unlock()
	t.recordActivity(llm.ActivityRecord{Kind: "native_tool", ID: id, Tool: name, StartedAt: activity.started, Reason: "tool_use"})
}

func (t *turn) endNativeTool(id, reason string) {
	t.mu.Lock()
	activity, exists := t.nativeTools[id]
	delete(t.nativeTools, id)
	ended := t.now().UTC()
	t.mu.Unlock()
	if exists {
		t.recordActivity(llm.ActivityRecord{Kind: "native_tool", ID: id, Tool: activity.tool, StartedAt: activity.started, EndedAt: ended, Reason: reason})
	}
}

func (t *turn) endNativeToolCalls() {
	t.mu.Lock()
	var ids []string
	for id := range t.nativeTools {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	reason := "result_unavailable"
	if t.ctx.Err() != nil {
		reason = context.Cause(t.ctx).Error()
	}
	for _, id := range ids {
		t.endNativeTool(id, reason)
	}
}
