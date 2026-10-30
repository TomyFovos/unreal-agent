package host

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (s *Session) recordConversation(ctx context.Context, record llm.ConversationRecord) error {
	return (&serializedStore{Session: s}).AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{
		Version: 1, Kind: sessionstore.HostContext,
		Context: &sessionstore.ContextRecord{Kind: "provider_conversation", Provider: record.Provider, Conversation: &record},
	})
}

func (s *Session) recordActivity(ctx context.Context, record llm.ActivityRecord) error {
	return (&serializedStore{Session: s}).AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{
		Version: 1, Kind: sessionstore.HostActivity, Activity: &record,
	})
}

// RecordActivity appends execution metadata through the owning Session writer.
func (s *Session) RecordActivity(ctx context.Context, record llm.ActivityRecord) error {
	return s.recordActivity(ctx, record)
}

// Idle has its own progress lifetime, independent of the completed Respond.
// It leaves ModelBusy false so input and child notifications remain usable.
func (a *progressAdapter) Idle(ctx context.Context, r llm.Request, o llm.RequestOptions) {
	idle, ok := a.next.(llm.IdleAdapter)
	if !ok {
		return
	}
	s := a.session
	s.mu.Lock()
	if !s.running || ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.progressEpoch++
	epoch, turn := s.progressEpoch, s.currentTurn()
	o.ConversationTokens = s.conversationTokensLocked()
	s.mu.Unlock()
	o.RecordConversation = s.recordConversation
	o.RecordActivity = s.recordActivity
	done := false // protected by s.mu, including late provider callbacks
	clear := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.progress != nil && s.progress.Epoch == epoch {
			s.progress = nil
			s.broadcast(Event{Kind: "progress"})
		}
	}
	defer func() {
		s.mu.Lock()
		done = true
		s.mu.Unlock()
		clear()
	}()
	o.Progress = func(p llm.Progress) {
		if p.Reset {
			clear()
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !done && ctx.Err() == nil && s.running && s.progressEpoch == epoch && s.currentTurn() == turn {
			s.progress = &Progress{TurnID: turn, Epoch: epoch, Attempt: p.Attempt, Mode: p.Phase}
			s.broadcast(Event{Kind: "progress", Progress: s.progress})
		}
	}
	idle.Idle(ctx, r, o)
}
