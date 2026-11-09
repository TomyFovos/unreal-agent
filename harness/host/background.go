package host

import (
	"context"
	"sync"
)

// backgroundWork owns optional adapter work beyond a single request or idle
// interval. Admission and shutdown share a lock so no Add can follow Wait.
type backgroundWork struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wait   sync.WaitGroup
}

func newBackgroundWork(parent context.Context) *backgroundWork {
	ctx, cancel := context.WithCancel(parent)
	return &backgroundWork{ctx: ctx, cancel: cancel}
}

func (b *backgroundWork) start(work func(context.Context)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.ctx.Err() != nil {
		return false
	}
	b.wait.Go(func() { work(b.ctx) })
	return true
}

func (b *backgroundWork) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.wait.Wait()
}
