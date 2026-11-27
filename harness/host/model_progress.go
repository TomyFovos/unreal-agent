package host

import (
	"maps"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

type ModelPhase string

const (
	ModelRequesting ModelPhase = "requesting"
	ModelStreaming  ModelPhase = "streaming"
)

func (p *Progress) modelPhase() ModelPhase {
	if p != nil && !p.Done {
		switch p.Mode {
		case "streaming":
			if p.Text != "" {
				return ModelStreaming
			}
			return ModelRequesting
		case "completed_response":
			return ModelRequesting
		}
	}
	return ""
}

// ModelProgress relays request phases and usage-limit resets across child process boundaries.
// Children is keyed by immediate child ID so observations can follow the
// same lineage as their canonical start records.
type ModelProgress struct {
	Phase        ModelPhase                   `json:",omitzero"`
	UsageRetryAt time.Time                    `json:",omitzero"`
	Children     map[session.ID]ModelProgress `json:",omitzero"`
}

func (p ModelProgress) empty() bool {
	return p.Phase == "" && p.UsageRetryAt.IsZero() && len(p.Children) == 0
}

func (s *Session) modelProgressChangedLocked() {
	if s.modelProgressChanged != nil {
		close(s.modelProgressChanged)
		s.modelProgressChanged = nil
	}
}

// WatchModelProgress returns an observation for the child transport. Its
// nested maps are immutable and can be encoded after the Session unlocks.
func (s *Session) WatchModelProgress() (ModelProgress, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelProgressChanged == nil {
		s.modelProgressChanged = make(chan struct{})
	}
	p := s.modelProgress
	p.Children = maps.Clone(p.Children)
	return p, s.modelProgressChanged
}

// SetChildModelProgress takes ownership of the snapshot decoded from the
// current child channel. Subtrees remain immutable after publication.
func (s *Session) SetChildModelProgress(id session.ID, progress ModelProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if progress.empty() {
		if _, ok := s.modelProgress.Children[id]; !ok {
			return
		}
		delete(s.modelProgress.Children, id)
	} else {
		if s.modelProgress.Children == nil {
			s.modelProgress.Children = make(map[session.ID]ModelProgress)
		}
		s.modelProgress.Children[id] = progress
	}
	s.modelProgressChangedLocked()
}

func (s *Session) ChildModelPhase(path []session.ID) ModelPhase {
	return s.ChildModelProgress(path).Phase
}

// ChildModelProgress returns the live model state along a validated child path.
func (s *Session) ChildModelProgress(path []session.ID) ModelProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress := s.modelProgress
	for _, id := range path {
		progress = progress.Children[id]
	}
	if progress.Phase != ModelRequesting && progress.Phase != ModelStreaming {
		progress.Phase = ""
	}
	return ModelProgress{Phase: progress.Phase, UsageRetryAt: progress.UsageRetryAt}
}
