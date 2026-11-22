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

type stepID struct{ parent, message string }

type usageStep struct {
	id    stepID
	model string
	usage usage
}

// result.usage covers this execution, including discarded output and recovery
// attempts. result.modelUsage also includes prior resumed executions. Use the
// current streams and assistant messages to attribute the execution total.
func (t *turn) recordTurnUsage(result *frame) {
	t.mu.Lock()
	model := t.model
	messages := t.messages
	t.mu.Unlock()
	var streams []usageStep
	if t.streams != nil {
		t.streams.mu.Lock()
		if t.streams.mainModel != "" {
			model = t.streams.mainModel
		}
		streams = slices.Clone(t.streams.usage)
		t.streams.mu.Unlock()
	}
	models := map[string]string{"": model}
	for id, step := range messages {
		if step.model != "" && step.model != "<synthetic>" {
			models[id.parent] = first(models[id.parent], step.model)
		}
	}
	for _, step := range streams {
		models[step.id.parent] = first(models[step.id.parent], step.model)
	}
	model = models[""]
	totals := make(map[string]llm.Usage)
	var main, nested llm.Usage
	var seenMain, seenNested bool
	add := func(step usageStep) {
		if step.model == "<synthetic>" {
			return
		}
		u := reportedUsage(step.usage)
		if !step.usage.inputReported {
			u.Unknown = append(u.Unknown, llm.UsageInput)
		}
		if !step.usage.outputReported {
			u.Unknown = append(u.Unknown, llm.UsageOutput)
		}
		totals[step.model] = addUsage(totals[step.model], u)
		if step.id.parent == "" {
			main = addUsage(main, u)
			seenMain = true
		} else {
			nested = addUsage(nested, u)
			seenNested = true
		}
	}
	seen := make(map[stepID]bool)
	anonymous := make(map[[2]string]bool)
	for _, step := range streams {
		step.model = first(step.model, messages[step.id].model, models[step.id.parent])
		if step.id.message == "" {
			anonymous[[2]string{step.id.parent, step.model}] = true
		} else if seen[step.id] {
			continue
		} else {
			seen[step.id] = true
		}
		add(step)
	}
	for id, step := range messages {
		step.model = first(step.model, models[id.parent])
		if !seen[id] && !anonymous[[2]string{id.parent, step.model}] {
			add(step)
		}
	}
	if !seenMain {
		main = unknownUsage()
	}
	if result.Usage != nil {
		u := reportedUsage(*result.Usage)
		// Reconcile the execution total with observed requests. Scope is
		// independent of model name: an Agent can use the same model as main.
		if !t.sawNested {
			main = u
		} else if seenNested {
			main = remainingUsage(u, nested, main)
		}
		if !t.sawNested || seenNested {
			var other llm.Usage
			for name, measured := range totals {
				if name != model {
					other = addUsage(other, measured)
				}
			}
			totals[model] = remainingUsage(u, other, totals[model])
		}
		// The per-model rows are a breakdown of the execution, not extra usage.
		t.recordActivity(llm.ActivityRecord{Kind: "claude_usage", ID: t.opt.Identity.TurnID, Provider: ProviderID, Model: model, EndedAt: t.now().UTC(), Usage: &u})
		t.resultUsage = &main
	} else if t.sawNested && seenMain {
		t.resultUsage = &main
	}
	for model, u := range totals {
		if model != "" && model != "<synthetic>" {
			t.recordActivity(llm.ActivityRecord{Kind: "provider_usage", ID: t.opt.Identity.TurnID, Provider: ProviderID, Model: model, EndedAt: t.now().UTC(), Usage: &u})
		}
	}
}

func remainingUsage(total, used, measured llm.Usage) llm.Usage {
	for _, field := range []struct {
		name llm.UsageField
		dst  *int64
		used int64
		min  int64
	}{
		{llm.UsageInput, &total.InputTokens, used.InputTokens, measured.InputTokens},
		{llm.UsageCachedInput, &total.CachedInputTokens, used.CachedInputTokens, measured.CachedInputTokens},
		{llm.UsageCacheWriteInput, &total.CacheWriteInputTokens, used.CacheWriteInputTokens, measured.CacheWriteInputTokens},
		{llm.UsageOutput, &total.OutputTokens, used.OutputTokens, measured.OutputTokens},
		{llm.UsageReasoning, &total.ReasoningTokens, used.ReasoningTokens, measured.ReasoningTokens},
	} {
		if slices.Contains(used.Unknown, field.name) {
			if !slices.Contains(total.Unknown, field.name) {
				total.Unknown = append(total.Unknown, field.name)
			}
			*field.dst = field.min
			continue
		}
		*field.dst = max(field.min, *field.dst-field.used)
	}
	return total
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
