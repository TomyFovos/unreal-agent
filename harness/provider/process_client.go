package provider

import (
	"context"
	"sync"
)

// processRequests gives an executable adapter ownership of its in-flight
// requests. The Coordinator can stop accepting a response before its worker has
// exited; runtime Close must still drain processes and pipes before returning.
// HTTP providers retain their existing lifecycle.
type processRequests struct {
	mu      sync.Mutex
	closed  bool
	cancels map[*context.CancelFunc]context.CancelFunc
	wait    sync.WaitGroup
}

func (p *processRequests) begin(ctx context.Context) (context.Context, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(ctx)
	p.cancels[&cancel] = cancel
	p.wait.Add(1)
	return ctx, func() {
		cancel()
		p.mu.Lock()
		delete(p.cancels, &cancel)
		p.mu.Unlock()
		p.wait.Done()
	}, nil
}

func (p *processRequests) close() {
	p.mu.Lock()
	p.closed = true
	for _, cancel := range p.cancels {
		cancel()
	}
	p.mu.Unlock()
	p.wait.Wait()
}
