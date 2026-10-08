package host

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (s *Session) verifyRuntimeSelections() error {
	var revision, active uint64
	var pending *sessionstore.RuntimeSelection
	for _, item := range s.items {
		if item.Kind == sessionstore.ItemFork {
			revision, active, pending = 0, 0, nil
		}
		if r, ok := item.Data.(sessionstore.HostRecord); ok {
			switch r.Kind {
			case sessionstore.HostRuntimeSelection:
				if r.Validate() != nil || r.Selection.Revision != revision+1 {
					return errors.New("invalid runtime selection history")
				}
				revision = r.Selection.Revision
				pending = r.Selection
			case sessionstore.HostRuntimeApplied:
				if r.Validate() != nil || pending == nil || *pending != *r.Selection {
					return errors.New("invalid runtime activation history")
				}
				active = r.Selection.Revision
				pending = nil
			}
		}
		if turn, ok := item.Data.(session.Turn); ok && turn.RuntimeRevision != active {
			return errors.New("turn runtime revision does not match canonical activation")
		}
	}
	return nil
}

// ActiveRuntimeSelection lets a factory replay the applied selection without
// relaxing validation of the immutable creation configuration.
func ActiveRuntimeSelection(ctx context.Context) *sessionstore.RuntimeSelection {
	s, ok := ctx.Value(sessionContextKey{}).(*Session)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selection == nil {
		return nil
	}
	copy := *s.selection
	return &copy
}
func (s *Session) RuntimeSelection() (active, pending *sessionstore.RuntimeSelection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selection != nil {
		copy := *s.selection
		active = &copy
	}
	if s.pendingSelection != nil {
		copy := *s.pendingSelection
		pending = &copy
	}
	return
}

// SelectRuntime persists an idempotent, optimistic-concurrency intent. It never
// touches an executing builder/client or submits an input to the model.
func (s *Session) SelectRuntime(ctx context.Context, generation string, expected uint64, choice sessionstore.RuntimeSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation != s.Generation {
		return ErrStaleGeneration
	}
	if choice.Validate() != nil || choice.RequestID == "" {
		return errors.New("invalid runtime selection")
	}
	if s.runtime.ValidateSelection != nil {
		if err := s.runtime.ValidateSelection(choice); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return ErrStopped
	}
	if s.runtime.ApplySelection == nil || s.selection == nil || s.runtime.ValidateSelection == nil && choice.Provider != s.selection.Provider {
		return errors.New("runtime switching unavailable")
	}
	for i := len(s.items) - 1; i >= 0; i-- {
		if s.items[i].Kind == sessionstore.ItemFork {
			break
		}
		if r, ok := s.items[i].Data.(sessionstore.HostRecord); ok && r.Kind == sessionstore.HostRuntimeSelection && r.Selection.RequestID == choice.RequestID {
			old := *r.Selection
			choice.Revision = old.Revision
			if choice == old {
				return nil
			}
			return ErrConflict
		}
	}
	revision := s.selection.Revision
	if s.pendingSelection != nil {
		revision = s.pendingSelection.Revision
	}
	if expected != revision || revision == ^uint64(0) {
		return ErrConflict
	}
	choice.Revision = revision + 1
	return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeSelection, Selection: &choice})
}

func (s *Session) beginTurn(ctx context.Context, boundary bool) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime.ApplySelection == nil || s.selection == nil {
		return 0, nil
	}
	if boundary && s.pendingSelection != nil {
		choice := *s.pendingSelection
		// Commit activation before effects. A crash here replays the activation
		// before the next model request; no old turn is rewritten.
		if err := s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: &choice}); err != nil {
			return 0, err
		}
	}
	if err := s.runtime.ApplySelection(*s.selection); err != nil {
		return 0, err
	}
	return s.selection.Revision, nil
}
