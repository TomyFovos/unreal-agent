package claudeagent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const DefaultCompactThresholdTokens int64 = 200_000

const compactPrompt = "/compact Preserve the goal, decisions made, questions asked of the user and anything awaiting their reply, active child handles and their roles, files being changed, and the next actions to take."

type compaction struct {
	before int64
	after  *int64
	usage  *llm.Usage
}

func (c *compaction) handle(f frame) {
	if f.Type == "result" && f.Usage != nil {
		u := reportedUsage(*f.Usage)
		c.usage = &u
	}
	if f.Type == "system" && f.Subtype == "compact_boundary" && f.CompactMetadata != nil {
		c.before = f.CompactMetadata.PreTokens
		c.after = f.CompactMetadata.PostTokens
	}
	if f.Type == "result" && c.after == nil && f.Usage != nil && f.Usage.inputTokens() > 0 {
		value := f.Usage.inputTokens()
		c.after = &value
	}
}

func (c *Client) lastContextTokens(session string, opt llm.RequestOptions) int64 {
	if opt.ConversationTokens != nil {
		return opt.ConversationTokens[ProviderID]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.contextTokens[session]
}

type conversationLock struct {
	gate  chan struct{}
	users int
}

// Provider factories create a fresh Client per request. Share conversation
// ownership across those instances, releasing entries after the last waiter.
var conversationLocks = struct {
	sync.Mutex
	entries map[string]*conversationLock
}{entries: make(map[string]*conversationLock)}

// lockConversation serializes complete process lifetimes, including reaping,
// for each Claude Code conversation. Cancellation also interrupts lock waits.
func (c *Client) lockConversation(ctx context.Context, session string) (func(), error) {
	key := c.config.Directory + "\x00" + session
	conversationLocks.Lock()
	lock := conversationLocks.entries[key]
	if lock == nil {
		lock = &conversationLock{gate: make(chan struct{}, 1)}
		conversationLocks.entries[key] = lock
	}
	lock.users++
	conversationLocks.Unlock()
	release := func() {
		conversationLocks.Lock()
		defer conversationLocks.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(conversationLocks.entries, key)
		}
	}
	select {
	case lock.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock.gate
			release()
			return nil, err
		}
		return func() { <-lock.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// Idle is called by the Session owner only after the turn and its tools finish.
// It has its own process, resuming the same Claude Code conversation.
func (c *Client) Idle(ctx context.Context, request llm.Request, opt llm.RequestOptions) {
	session := sessionUUID(c.config.Namespace, opt.Identity.SessionID)
	unlock, err := c.lockConversation(ctx, session)
	if err != nil {
		return
	}
	defer unlock()
	if before := c.lastContextTokens(session, opt); before > c.config.CompactThresholdTokens {
		c.compact(ctx, request, opt, session, before)
	}
}

// compact is never run at a tool/steering batch. The caller owns the
// conversation lock until this process has exited and its record is saved.
func (c *Client) compact(ctx context.Context, request llm.Request, opt llm.RequestOptions, session string, before int64) {
	started := time.Now().UTC()
	if opt.RecordActivity != nil {
		if err := opt.RecordActivity(ctx, llm.ActivityRecord{Kind: "claude_compact", ID: started.Format(time.RFC3339Nano), StartedAt: started, Provider: ProviderID}); err != nil {
			slog.Warn("record Claude compaction start", "error", err)
		}
	}
	if opt.Progress != nil {
		opt.Progress(llm.Progress{Attempt: 1, Phase: "compacting"})
		defer opt.Progress(llm.Progress{Attempt: 1, Reset: true})
	}
	state := &compaction{before: before}
	_, err := c.run(ctx, request, opt, session, true, compactPrompt, state)
	record := llm.ConversationRecord{Provider: ProviderID, BeforeTokens: state.before, Phase: "compact", StartedAt: started, EndedAt: time.Now().UTC(), Usage: state.usage}
	if err != nil && ctx.Err() != nil {
		record.InputTokens, record.InputKnown, record.Interrupted = before, true, true
		record.InterruptReason = context.Cause(ctx).Error()
		slog.Info("Claude Code conversation compaction interrupted", "session", opt.Identity.SessionID)
	} else if err != nil {
		record.InputTokens, record.InputKnown, record.Error = before, true, clip(err.Error(), 2000)
		slog.Warn("Claude Code conversation compaction failed; continuing with the original input", "session", opt.Identity.SessionID, "error", err)
	} else {
		record.Compacted = true
		if state.after != nil {
			record.InputTokens, record.InputKnown = *state.after, true
		}
	}
	c.recordConversation(context.WithoutCancel(ctx), opt, session, record)
}

func (c *Client) recordConversation(ctx context.Context, opt llm.RequestOptions, session string, record llm.ConversationRecord) {
	c.mu.Lock()
	delete(c.contextTokens, session)
	if record.InputKnown {
		c.contextTokens[session] = record.InputTokens
	}
	c.mu.Unlock()
	if opt.RecordConversation != nil {
		if err := opt.RecordConversation(ctx, record); err != nil {
			slog.Warn("record Claude Code conversation context", "session", opt.Identity.SessionID, "error", err)
		}
	}
}
