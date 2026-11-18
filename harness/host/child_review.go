package host

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
)

// RecordChildReview appends through the parent's single canonical writer.
// A review is evidence, not a gate on completing or starting other work.
func (s *Session) RecordChildReview(ctx context.Context, generation string, review sessionstore.ChildReview) (Receipt, error) {
	record := sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostChildReview, ChildReview: &review}
	if err := record.Validate(); err != nil {
		return Receipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.Generation {
		return Receipt{}, ErrStaleGeneration
	}
	for _, item := range s.items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.ChildReview != nil && r.ChildReview.InputID == review.InputID {
			if *r.ChildReview != review {
				return Receipt{}, ErrConflict
			}
			return Receipt{ID: review.InputID, Sequence: item.Sequence}, nil
		}
	}
	op, ok := s.operations[operation.ID(review.Handle)]
	state, err := operation.DecodeRemoteJobState(op)
	var plan struct{ Action, ParentID string }
	if !ok || err != nil || state.Plan.Type != "subagent" || json.Unmarshal(state.Plan.Data, &plan) != nil || plan.Action != "start" || plan.ParentID != string(s.ID) || review.ChildID != string(identity.ChildID(s.ID, op.ID)) {
		return Receipt{}, fmt.Errorf("child ownership mismatch")
	}
	if err := s.store.AppendHostRecord(ctx, s.ID, record); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: review.InputID, Sequence: s.items[len(s.items)-1].Sequence}, nil
}
