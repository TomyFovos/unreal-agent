package host

import (
	"testing"
	"time"
)

// A gateway subscriber can consume the last buffered event at the same time
// broadcast evicts it for falling behind. Eviction must never wait for it:
// broadcast holds Session.mu, shared by Submit, Inspect and progress.
func TestBroadcastEvictionDoesNotBlockOnConcurrentDrain(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s := &Session{subscribers: map[uint64]chan Event{}}
		for range 50000 {
			ch := make(chan Event, 1)
			ch <- Event{Kind: "progress"}
			s.subscribers[0] = ch
			read := make(chan struct{})
			go func() {
				for range ch {
				}
				close(read)
			}()
			s.mu.Lock()
			s.broadcast(Event{Kind: "item"})
			if _, ok := s.subscribers[0]; ok {
				close(ch)
				delete(s.subscribers, 0)
			}
			s.mu.Unlock()
			<-read
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast blocked while evicting a concurrently drained subscriber; Session.mu remains locked")
	}
}
