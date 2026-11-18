package contextstate

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

// Projection includes replay bookkeeping outside the thirteen-slot State.
// Checkpoints keep event identities so a retry across restore remains idempotent.
type Projection struct {
	State           State                     `json:"state"`
	Applied         map[string]record.Locator `json:"applied,omitempty"`
	SettledChildren map[string]ChildCount     `json:"settled_children,omitempty"`
}

// Reduce calculates a detached next state. Apply implements the same transition
// for an exclusively owned replay projection without copying untouched slots.
func Reduce(previous Projection, event Event) (Projection, error) {
	if _, exists := previous.Applied[event.ID]; exists {
		return previous, nil
	}
	data, err := json.Marshal(previous)
	if err != nil {
		return previous, err
	}
	var next Projection
	if err := json.Unmarshal(data, &next); err != nil {
		return previous, err
	}
	if err := next.Apply(event); err != nil {
		return previous, err
	}
	return next, nil
}

func (p *Projection) Apply(e Event) error {
	if _, exists := p.Applied[e.ID]; exists {
		return nil
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Version == Version {
		if err := p.apply(e); err != nil {
			return fmt.Errorf("context event %q (%s): %w", e.ID, e.Kind, err)
		}
	}
	if p.State.Version == 0 {
		p.State.Version = Version
	}
	put(&p.Applied, e.ID, e.Record)
	return nil
}

func (p *Projection) apply(e Event) error {
	s := &p.State
	switch e.Kind {
	case SessionChanged:
		return applyValue(e, func(v Session) {
			v.Evidence = e.evidence(v.Evidence)
			s.Session = v
		})
	case RequestChanged:
		return applyValue(e, func(v Request) { s.updateRequest(e, v) })
	case RuleRecorded:
		return applyValue(e, func(v Rule) {
			v.Evidence = e.evidence(v.Evidence, v.Locator)
			put(&s.Rules, v.ID, v)
		})
	case PlanChanged:
		return applyValue(e, func(v PlanStep) { s.Plan.update(e, v) })
	case DecisionRecorded:
		return applyValue(e, func(v Decision) {
			if v.Locator == "" {
				v.Locator = e.Record
			}
			v.Evidence = e.evidence(v.Evidence, append(slices.Clone(v.Basis), v.Locator)...)
			put(&s.Decisions, v.ID, v)
		})
	case ChildChanged:
		return applyValue(e, func(v ChildUpdate) { p.updateChild(e, v) })
	case ApprovalChanged:
		return applyValue(e, func(v ApprovalUpdate) {
			if v.AlreadyClosed {
				evidence := e.evidence(v.Evidence, v.Locator)
				s.Approvals.Processed.addSources(evidence)
				s.Waiting.Processed.addSources(evidence)
				return
			}
			s.updateApproval(e, v.Approval)
		})
	case WaitChanged:
		return applyValue(e, func(v WaitUpdate) {
			v.Evidence = e.evidence(v.Evidence, v.Question)
			s.updateWait(v.Wait, v.Resolved)
		})
	case QuestionChanged:
		return applyValue(e, func(v Question) { s.updateQuestion(e, v) })
	case ArtifactChanged:
		return applyValue(e, func(v Artifact) { s.updateArtifact(e, v) })
	case IssueChanged:
		return applyValue(e, func(v Issue) {
			v.Evidence = e.evidence(v.Evidence, v.Locator)
			updateCurrent(&s.Issues, v.ID, v, v.Status == IssueClosed, v.Evidence, s.Issues.Current[v.ID].Evidence)
		})
	case RecentChanged:
		return applyValue(e, func(items []RecentEvent) {
			for i := range items {
				items[i].Evidence = e.evidence(items[i].Evidence, items[i].Locator)
			}
			s.RecentEvents = items
		})
	case RecentObserved:
		return applyValue(e, func(v RecentEvent) {
			v.Evidence = e.evidence(v.Evidence, v.Locator)
			s.RecentEvents = append(s.RecentEvents, v)
		})
	case MetricsChanged:
		return applyValue(e, func(v Metrics) {
			v.Evidence = e.evidence(v.Evidence)
			if v.LastBoundary != nil {
				v.LastBoundary.Evidence = e.evidence(v.LastBoundary.Evidence, v.LastBoundary.Locator)
			}
			s.Metrics = v
			s.settleArtifacts()
		})
	case BoundaryObserved:
		return applyValue(e, func(v Boundary) {
			v.Evidence = e.evidence(v.Evidence, v.Locator)
			s.Metrics.LastBoundary = &v
			s.settleArtifacts()
		})
	}
	return nil
}

func applyValue[T any](e Event, use func(T)) error {
	var value T
	if err := json.Unmarshal(e.Data, &value); err != nil {
		return err
	}
	use(value)
	return nil
}

func put[K comparable, V any](m *map[K]V, key K, value V) {
	if *m == nil {
		*m = make(map[K]V)
	}
	(*m)[key] = value
}

func (e Event) evidence(value Evidence, references ...record.Locator) Evidence {
	sources := append(slices.Clone(value.Sources), e.Sources...)
	sources = append(sources, e.Record)
	sources = append(sources, references...)
	sources = slices.DeleteFunc(sources, func(l record.Locator) bool { return l == "" })
	slices.Sort(sources)
	return Evidence{Sources: slices.Compact(sources)}
}

func (p *Processed) add(evidence ...Evidence) {
	p.Count++
	p.addSources(evidence...)
}

func (p *Processed) addSources(evidence ...Evidence) {
	for _, value := range evidence {
		for _, locator := range value.Sources {
			put(&p.Sources, locator, struct{}{})
		}
	}
}

func updateCurrent[T any](c *Entries[T], key string, value T, done bool, evidence, earlier Evidence) {
	if !done {
		put(&c.Current, key, value)
		return
	}
	delete(c.Current, key)
	c.Processed.add(evidence, earlier)
}

func (s *State) updateRequest(e Event, v Request) {
	v.Evidence = e.evidence(v.Evidence, append(slices.Clone(v.Aliases), v.Locator, v.Original)...)
	if v.Origin == HumanInput && v.Text != "" {
		line, _, newline := strings.Cut(v.Text, "\n")
		if newline {
			line = strings.TrimSuffix(line, "\r")
		}
		v.FirstLine, v.Chars = line, utf8.RuneCountInString(v.Text)
	}
	if v.Status != RequestActive || v.Origin != HumanInput {
		v.Text = ""
	}
	updateCurrent(&s.Requests, v.ID, v, v.Status == RequestDone, v.Evidence, s.Requests.Current[v.ID].Evidence)
}

func (p *Plan) update(e Event, v PlanStep) {
	v.Evidence = e.evidence(v.Evidence)
	index := slices.IndexFunc(p.Steps, func(step PlanStep) bool { return step.ID == v.ID })
	if v.Status == PlanDone || v.Status == PlanDropped {
		past := Evidence{}
		if index >= 0 {
			past = p.Steps[index].Evidence
			p.Steps = slices.Delete(p.Steps, index, index+1)
		}
		p.Processed.add(past, v.Evidence)
		return
	}
	if index < 0 {
		p.Steps = append(p.Steps, v)
	} else {
		p.Steps[index] = v
	}
	slices.SortFunc(p.Steps, func(a, b PlanStep) int {
		if a.Position != b.Position {
			return cmp.Compare(a.Position, b.Position)
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

func (p *Projection) updateChild(e Event, update ChildUpdate) {
	s := &p.State
	v := update.Child
	v.Evidence = e.evidence(v.Evidence, append(slices.Clone(v.UnansweredQuestions), v.LastEvent, v.Report)...)
	if v.Accept == "" {
		v.Accept = AcceptPending
	}
	earlier := s.Children.Current[v.Handle]
	done := v.Accept == AcceptAccepted || v.Accept == AcceptRejected || update.CancelRecorded ||
		(v.State == ChildCancelled && (v.EndedBy == EndedByCoordinatorCancel || v.EndedBy == EndedByUserCancel))
	if v.State == ChildFinished || v.State == ChildFailed || v.State == ChildCancelled {
		s.finishWait(Wait{ID: v.Handle, Kind: WaitChildCompletion, Evidence: v.Evidence})
	}
	group := ChildCount{Template: v.Template, Model: v.Model, EndedBy: v.EndedBy}
	if settled, ok := p.SettledChildren[v.Handle]; ok {
		s.Children.Processed.addSources(v.Evidence)
		if group.EndedBy != "" && group != settled {
			s.Children.count(settled, -1)
			s.Children.count(group, 1)
			p.SettledChildren[v.Handle] = group
		}
		return
	}
	updateCurrent(&s.Children.Entries, v.Handle, v, done, v.Evidence, earlier.Evidence)
	if done {
		if group.Template == "" {
			group.Template = earlier.Template
		}
		if group.Model == "" {
			group.Model = earlier.Model
		}
		if group.EndedBy == "" {
			group.EndedBy = earlier.EndedBy
		}
		put(&p.SettledChildren, v.Handle, group)
		s.Children.count(group, 1)
		s.settleArtifacts()
	}
}

func (c *Children) count(group ChildCount, delta int) {
	i := slices.IndexFunc(c.ByEnding, func(row ChildCount) bool {
		return row.Template == group.Template && row.Model == group.Model && row.EndedBy == group.EndedBy
	})
	if i >= 0 {
		row := &c.ByEnding[i]
		row.Count = uint64(int64(row.Count) + int64(delta))
		if row.Count == 0 {
			c.ByEnding = slices.Delete(c.ByEnding, i, i+1)
		}
		return
	}
	group.Count = 1
	c.ByEnding = append(c.ByEnding, group)
	slices.SortFunc(c.ByEnding, func(a, b ChildCount) int {
		if a.Template != b.Template {
			return cmp.Compare(a.Template, b.Template)
		}
		if a.Model != b.Model {
			return cmp.Compare(a.Model, b.Model)
		}
		return cmp.Compare(a.EndedBy, b.EndedBy)
	})
}

func (s *State) updateApproval(e Event, v Approval) {
	v.Evidence = e.evidence(v.Evidence, v.Locator)
	done := v.Decision != ""
	updateCurrent(&s.Approvals, v.ID, v, done, v.Evidence, s.Approvals.Current[v.ID].Evidence)
	s.updateWait(Wait{ID: v.ID, Kind: WaitApproval, TS: v.TS, Handle: v.Handle, Evidence: v.Evidence}, done)
}

func (s *State) updateQuestion(e Event, v Question) {
	if v.Locator == "" {
		v.Locator = e.Record
	}
	v.Evidence = e.evidence(v.Evidence, v.Locator, v.Answer)
	done := v.Status == QuestionAnswered
	updateCurrent(&s.QuestionsToUser, v.ID, v, done, v.Evidence, s.QuestionsToUser.Current[v.ID].Evidence)
	s.updateWait(Wait{ID: v.ID, Kind: WaitUserAnswer, TS: v.TS, Question: v.Locator, Evidence: v.Evidence}, done)
}

func waitKey(v Wait) string { return string(v.Kind) + ":" + v.ID }

func (s *State) updateWait(v Wait, done bool) {
	if done {
		s.finishWait(v)
	} else {
		put(&s.Waiting.Current, waitKey(v), v)
	}
}

func (s *State) finishWait(v Wait) {
	key := waitKey(v)
	if earlier, exists := s.Waiting.Current[key]; exists {
		updateCurrent(&s.Waiting, key, v, true, v.Evidence, earlier.Evidence)
	}
}

func (s *State) activeArtifact(v Artifact) bool {
	if v.LastWriter == "self" {
		return s.Metrics.LastBoundary == nil || !v.TS.Before(s.Metrics.LastBoundary.TS)
	}
	_, current := s.Children.Current[v.LastWriter]
	return current
}

func (s *State) updateArtifact(e Event, v Artifact) {
	v.Evidence = e.evidence(v.Evidence, v.Locator)
	if earlier, exists := s.Artifacts.Current[v.Path]; exists && earlier.Locator != v.Locator {
		s.Artifacts.Processed.add(earlier.Evidence)
		delete(s.Artifacts.Current, v.Path)
	}
	updateCurrent(&s.Artifacts, v.Path, v, !s.activeArtifact(v), v.Evidence, s.Artifacts.Current[v.Path].Evidence)
}

func (s *State) settleArtifacts() {
	for path, v := range s.Artifacts.Current {
		if !s.activeArtifact(v) {
			updateCurrent(&s.Artifacts, path, v, true, v.Evidence, Evidence{})
		}
	}
}
