package contextstate

import (
	"maps"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

// After is the item sequence of an observation or the preceding item
// sequence for an operation checkpoint. Legacy checkpoints can lack a time.
type position struct {
	After uint64
	TS    time.Time
}

func itemSequence(loc record.Locator) uint64 {
	addr, _ := loc.Address()
	return addr.Sequence
}

func (p position) since(start position) bool {
	if p.After != 0 && start.After != 0 {
		return p.After >= start.After
	}
	return !p.TS.IsZero() && !start.TS.IsZero() && !p.TS.Before(start.TS)
}

func (s State) activeRequest() (position, bool) {
	var start position
	var found bool
	var latestHuman position
	for _, req := range s.Requests.Current {
		if req.Origin != HumanInput {
			continue
		}
		next := position{After: itemSequence(req.Locator), TS: req.TS}
		later := next.After > start.After || next.After == start.After && next.TS.After(start.TS)
		if req.Status == RequestActive && (!found || later) {
			start, found = next, true
		}
		if req.Status == "" && next.After != 0 && next.After == s.LastHumanInput {
			latestHuman = next
		}
	}
	if !found && latestHuman.After != 0 {
		return latestHuman, true
	}
	return start, found
}

func (c Child) ended() bool {
	return c.State == ChildFinished || c.State == ChildFailed || c.State == ChildCancelled
}

func (s State) activeArtifact(v Artifact) bool {
	if v.SelfWrite == nil && v.LastWriter == "self" {
		v.SelfWrite = &position{After: v.After, TS: v.TS}
	}
	if v.SelfWrite != nil {
		boundary := s.Metrics.LastBoundary
		if boundary == nil || v.SelfWrite.since(position{After: itemSequence(boundary.Locator), TS: boundary.TS}) {
			return true
		}
	}
	if _, current := s.Children.Current[v.LastWriter]; current {
		return true
	}
	return slices.ContainsFunc(v.ChildWriters, func(handle string) bool {
		_, current := s.Children.Current[handle]
		return current
	})
}

// Render projects the current state for L2. The replay state still holds
// the evidence and lets an explicit review or answer link an older record.
func (s State) Render(recentLimit int) State {
	start, active := s.activeRequest()
	s.Children.Current = maps.Clone(s.Children.Current)
	s.Children.ByEnding = slices.Clone(s.Children.ByEnding)
	for handle, child := range s.Children.Current {
		if child.ended() && child.ReportSeen && (!active || !(position{After: child.EndedAfter, TS: child.Ended}).since(start)) {
			delete(s.Children.Current, handle)
			s.Children.Processed.Count++
			s.Children.count(ChildCount{Template: child.Template, Model: child.Model, EndedBy: child.EndedBy}, 1)
			continue
		}
		child.EndedAfter, child.ReportSeen = 0, false
		s.Children.Current[handle] = child
	}

	s.Artifacts.Current = maps.Clone(s.Artifacts.Current)
	for path, artifact := range s.Artifacts.Current {
		if !s.activeArtifact(artifact) {
			delete(s.Artifacts.Current, path)
			s.Artifacts.Processed.Count++
			continue
		}
		artifact.After, artifact.SelfWrite, artifact.ChildWriters = 0, nil, nil
		s.Artifacts.Current[path] = artifact
	}

	s.QuestionsToUser.Current = maps.Clone(s.QuestionsToUser.Current)
	for id, question := range s.QuestionsToUser.Current {
		if question.Status != QuestionOpen || !question.Marker || itemSequence(question.Locator) <= s.LastHumanInput {
			delete(s.QuestionsToUser.Current, id)
			s.QuestionsToUser.Processed.Count++
			continue
		}
		question.Marker = false
		s.QuestionsToUser.Current[id] = question
	}
	s.Waiting.Current = maps.Clone(s.Waiting.Current)
	for key, wait := range s.Waiting.Current {
		if wait.Kind == WaitUserAnswer {
			if _, open := s.QuestionsToUser.Current[wait.ID]; !open {
				delete(s.Waiting.Current, key)
				s.Waiting.Processed.Count++
			}
		}
	}
	s.LastHumanInput = 0

	var recent []RecentEvent
	if recentLimit > 0 {
		recent = slices.Clone(s.RecentEvents[max(0, len(s.RecentEvents)-recentLimit):])
	} else {
		if boundary := s.Metrics.LastBoundary; boundary != nil {
			start = position{After: itemSequence(boundary.Locator), TS: boundary.TS}
			active = true
		}
		if active {
			for _, event := range s.RecentEvents {
				after := event.After
				if after == 0 {
					after = itemSequence(event.Locator)
				}
				if (position{After: after, TS: event.TS}).since(start) {
					recent = append(recent, event)
				}
			}
		}
	}
	for i := range recent {
		recent[i].After = 0
	}
	s.RecentEvents = recent

	if s.Session.ID != "" {
		for slot, processed := range s.ProcessedSlots() {
			if processed.Count > 0 {
				processed.Locator = record.Address{Session: s.Session.ID, Kind: record.ContextIndex, Section: slot}.Locator()
			}
			processed.Sources = nil
		}
	}
	return s
}

// ProcessedSlots supplies the counts and evidence for the L3 indexes.
func (s *State) ProcessedSlots() map[string]*Processed {
	return map[string]*Processed{
		"requests": &s.Requests.Processed, "plan": &s.Plan.Processed,
		"children": &s.Children.Processed, "artifacts": &s.Artifacts.Processed,
		"approvals": &s.Approvals.Processed, "waiting": &s.Waiting.Processed,
		"questions_to_user": &s.QuestionsToUser.Processed, "issues": &s.Issues.Processed,
	}
}
