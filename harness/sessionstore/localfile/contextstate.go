package localfile

import (
	"context"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// AppendContextEvent uses the same Session writer as the transcript. An event
// retry returns the original committed locator, including after restoration.
func (s *Store) AppendContextEvent(ctx context.Context, id session.ID, event contextstate.Event) (record.Locator, error) {
	head, size, err := s.loadWriteState(ctx, id)
	if err != nil {
		return "", err
	}
	if existing, ok := head.ContextProjection.Applied[event.ID]; ok {
		return existing, nil
	}
	event.Record = record.Item(string(id), uint64(head.itemSequence+1))
	if err := head.ContextProjection.Apply(event); err != nil {
		return "", err
	}
	item := head.appendItem(sessionstore.ItemContextEvent, event, time.Now().UTC())
	if err := s.append(id, head, size, recordItem, itemRecord{Item: item}); err != nil {
		return "", err
	}
	s.notifyObservers(id, item)
	return event.Record, nil
}

// ContextEventLocator checks the write head owned by the Session caller.
func (s *Store) ContextEventLocator(ctx context.Context, id session.ID, eventID string) (record.Locator, error) {
	head, _, err := s.loadWriteState(ctx, id)
	return head.ContextProjection.Applied[eventID], err
}

// ContextState returns a detached snapshot. An owning Session can use its
// existing restored write head. A read-only Store replays canonical history.
func (s *Store) ContextState(ctx context.Context, id session.ID) (contextstate.State, error) {
	if err := context.Cause(ctx); err != nil {
		return contextstate.State{}, err
	}
	if head, _, ok := s.getCachedWriteState(id); ok {
		return stateSnapshot(head.ContextProjection.State)
	}
	state, _, err := s.readState(ctx, id)
	if err != nil {
		return contextstate.State{}, err
	}
	return stateSnapshot(state.ContextProjection.State)
}

func stateSnapshot(s contextstate.State) (contextstate.State, error) {
	if s.Version == 0 {
		s.Version = contextstate.Version
	}
	return cloneRestoreValue(s)
}
