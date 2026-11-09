package claudeagent

import (
	"context"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// The cache lives for one hour. Compact a large idle conversation at 50
// minutes to avoid rewriting all of its context after that cache expires.
const (
	idleCompactDelay               = 50 * time.Minute
	idleCompactMinimumTokens int64 = 120_000
)

type idleCompactJob struct {
	generation uint64
	due        time.Time
	run        func(context.Context)
}

// Fields are protected by idleCompactions.Mutex. One worker owns timers and
// compaction for the lifetime of a conversation, independently of Client.Close.
type idleCompactState struct {
	wake       chan struct{}
	lastCall   time.Time
	generation uint64
	armed      uint64
	turns      int
	pending    *idleCompactJob
}

var idleCompactions = struct {
	sync.Mutex
	entries map[string]*idleCompactState
}{entries: make(map[string]*idleCompactState)}

func (s *idleCompactState) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *idleCompactState) beginTurn() {
	idleCompactions.Lock()
	defer idleCompactions.Unlock()
	s.generation++
	s.turns++
	s.pending = nil
	s.notify()
}

func (s *idleCompactState) endTurn() {
	idleCompactions.Lock()
	defer idleCompactions.Unlock()
	s.turns--
	s.lastCall = time.Now()
}

func (c *Client) idleCompactionState(session string, opt llm.RequestOptions) *idleCompactState {
	key := c.config.Directory + "\x00" + session
	idleCompactions.Lock()
	defer idleCompactions.Unlock()
	s := idleCompactions.entries[key]
	if s == nil && opt.StartBackground != nil {
		s = &idleCompactState{wake: make(chan struct{}, 1), lastCall: time.Now(), generation: 1}
		idleCompactions.entries[key] = s
		if !opt.StartBackground(func(ctx context.Context) { c.runIdleCompactions(ctx, key, session, s) }) {
			delete(idleCompactions.entries, key)
			return nil
		}
	}
	return s
}

// The injected action keeps timer/ownership tests independent of subprocesses.
// Production always supplies the existing compact process path.
func (c *Client) scheduleIdleCompaction(ctx context.Context, opt llm.RequestOptions, run func(context.Context, string, int64)) {
	if ctx.Err() != nil || opt.StartBackground == nil {
		return
	}
	session := sessionUUID(c.config.Namespace, opt.Identity.SessionID)
	before := c.lastContextTokens(session, opt)
	if before <= idleCompactMinimumTokens {
		return
	}
	s := c.idleCompactionState(session, opt)
	if s == nil {
		return
	}
	idleCompactions.Lock()
	defer idleCompactions.Unlock()
	if s.turns != 0 || s.armed == s.generation {
		return
	}
	s.armed = s.generation
	s.pending = &idleCompactJob{generation: s.generation, due: s.lastCall.Add(idleCompactDelay), run: func(ctx context.Context) { run(ctx, session, before) }}
	s.notify()
}

func (c *Client) runIdleCompactions(ctx context.Context, key, session string, s *idleCompactState) {
	defer func() {
		idleCompactions.Lock()
		defer idleCompactions.Unlock()
		s.pending = nil
		if idleCompactions.entries[key] == s {
			delete(idleCompactions.entries, key)
		}
	}()
	for ctx.Err() == nil {
		idleCompactions.Lock()
		job := s.pending
		var due time.Time
		if job != nil {
			due = job.due
		}
		idleCompactions.Unlock()
		var timer *time.Timer
		var ready <-chan time.Time
		if job != nil {
			timer = time.NewTimer(time.Until(due))
			ready = timer.C
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-ready:
			c.runIdleCompactJob(ctx, session, s, job)
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (c *Client) runIdleCompactJob(ctx context.Context, session string, s *idleCompactState, job *idleCompactJob) {
	unlock, err := c.lockConversation(ctx, session)
	if err != nil {
		return
	}
	defer unlock()
	idleCompactions.Lock()
	current := s.pending == job && s.generation == job.generation && s.turns == 0
	if current {
		s.pending = nil
	}
	idleCompactions.Unlock()
	if current && ctx.Err() == nil {
		// A new turn now waits on the conversation lock. Only the owner's
		// lifetime cancellation interrupts a compaction that has begun.
		job.run(ctx)
	}
}
