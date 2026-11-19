package claudeagent

import (
	"context"
	"sync"
)

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
