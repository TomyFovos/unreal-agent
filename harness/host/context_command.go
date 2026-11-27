package host

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
)

// ContextCommand is shared by the local Session CLI and the native tools.
// All writes use this Session's canonical writer and a caller-stable ID.
func (s *Session) ContextCommand(ctx context.Context, generation string, inputID string, cmd contextstate.Command) (contextstate.Result, error) {
	var result contextstate.Result
	decode := func(v any) error { return json.Unmarshal(cmd.Args, v, json.RejectUnknownMembers(true)) }
	switch cmd.Action {
	case contextstate.Show:
		var args struct{}
		if err := decode(&args); err != nil {
			return result, err
		}
		state, err := s.ContextState(ctx)
		if err == nil {
			state.Session.ID = string(s.ID)
			result.State = new(state.Render(0))
		}
		return result, err
	case contextstate.ShowRecord:
		var args contextstate.RecordSelection
		if err := decode(&args); err != nil {
			return result, err
		}
		read, previous := s.beginRecordRead("show")
		page, err := s.RecordShow(ctx, args.Locator, args.PageBytes)
		if err == nil {
			read.Bytes = new(int64(len(page.Bytes)))
			read.PreviousTailHits = previous.hits([]record.Locator{args.Locator})
		}
		s.recordRead(ctx, read)
		result.Page = &page
		return result, err
	case contextstate.FindRecord:
		var args contextstate.RecordSearch
		if err := decode(&args); err != nil {
			return result, err
		}
		read, previous := s.beginRecordRead("find")
		matches, err := s.RecordFind(ctx, record.Query(args))
		if err == nil {
			var bytes int64
			locators := make([]record.Locator, 0, len(matches))
			for _, match := range matches {
				bytes += int64(len(match.Excerpt))
				locators = append(locators, match.Locator)
			}
			read.Bytes = &bytes
			read.PreviousTailHits = previous.hits(locators)
		}
		s.recordRead(ctx, read)
		result.Matches = matches
		return result, err
	}
	if generation != s.Generation {
		return result, ErrStaleGeneration
	}
	if inputID == "" {
		return result, fmt.Errorf("context command requires an input ID")
	}
	switch cmd.Action {
	case contextstate.Accept, contextstate.Reject:
		var args contextstate.ReviewNote
		if err := decode(&args); err != nil {
			return result, err
		}
		decision := "accept"
		if cmd.Action == contextstate.Reject {
			decision = "reject"
		}
		child := identity.ChildID(s.ID, operation.ID(args.Handle))
		receipt, err := s.RecordChildReview(ctx, generation, sessionstore.ChildReview{
			InputID: inbox.ID("review:" + inputID), Handle: args.Handle, ChildID: string(child), Decision: decision, Reason: args.Why,
		})
		if err == nil {
			result = contextstate.Result{ID: args.Handle, Locator: record.Item(string(s.ID), uint64(receipt.Sequence))}
		}
		return result, err
	case contextstate.Decide:
		var args contextstate.DecisionNote
		if err := decode(&args); err != nil {
			return result, err
		}
		if strings.TrimSpace(args.Decision) == "" {
			return result, fmt.Errorf("decision text is required")
		}
		for _, loc := range args.Basis {
			if _, err := loc.Address(); err != nil {
				return result, err
			}
		}
		id := noteID(args.ID, inputID)
		note := contextstate.Decision{ID: id, TS: time.Now().UTC(), Decision: args.Decision, Reason: args.Why,
			Basis: args.Basis, Rejected: args.Rejected, Plan: args.Plan, Children: args.Children, Requests: args.Requests}
		return s.recordContextNote(ctx, inputID, id, contextstate.DecisionRecorded, note)
	case contextstate.AddRule:
		var args contextstate.RuleNote
		if err := decode(&args); err != nil {
			return result, err
		}
		raw, err := s.noteSource(ctx, args.Source, record.UserMessage)
		if err != nil {
			return result, err
		}
		if strings.TrimSpace(args.Excerpt) == "" || !bytes.Contains(raw, []byte(args.Excerpt)) {
			return result, fmt.Errorf("rule excerpt must quote its user message")
		}
		id := noteID(args.ID, inputID)
		note := contextstate.Rule{ID: id, Excerpt: args.Excerpt, Locator: args.Source}
		return s.recordContextNote(ctx, inputID, id, contextstate.RuleRecorded, note)
	case contextstate.SetPlan:
		var args contextstate.PlanNote
		if err := decode(&args); err != nil {
			return result, err
		}
		return s.setContextPlan(ctx, inputID, args)
	case contextstate.AskUser:
		var args contextstate.QuestionNote
		if err := decode(&args); err != nil {
			return result, err
		}
		if args.Source != "" {
			raw, err := s.noteSource(ctx, args.Source, record.AssistantMessage)
			if err != nil {
				return result, err
			}
			if args.Text == "" {
				args.Text = string(raw)
			}
		}
		if strings.TrimSpace(args.Text) == "" {
			return result, fmt.Errorf("question text or source is required")
		}
		id := noteID(args.ID, string(args.Source))
		id = noteID(id, inputID)
		line, _, _ := strings.Cut(args.Text, "\n")
		// The full note stays in its event, and the current slot uses one line.
		note := contextstate.QuestionUpdate{Question: contextstate.Question{
			ID: id, TS: time.Now().UTC(), Status: contextstate.QuestionOpen, FirstLine: strings.TrimSuffix(line, "\r"), Locator: args.Source,
		}, Text: args.Text}
		return s.recordContextNote(ctx, inputID, id, contextstate.QuestionChanged, note)
	case contextstate.AnswerUser:
		var args contextstate.AnswerNote
		if err := decode(&args); err != nil {
			return result, err
		}
		if _, err := s.noteSource(ctx, args.Source, record.UserMessage); err != nil {
			return result, err
		}
		return s.answerContextQuestion(ctx, inputID, args)
	case contextstate.Import:
		var args contextstate.ImportNote
		if err := decode(&args); err != nil {
			return result, err
		}
		return s.importContext(ctx, args)
	default:
		return result, fmt.Errorf("unknown context command %q", cmd.Action)
	}
}

type previousTail struct{ locators *[]record.Locator }

func (s *Session) beginRecordRead(action string) (sessionstore.RecordRead, previousTail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	read := sessionstore.RecordRead{Action: action, After: sessionstore.Sequence(len(s.items))}
	for i := len(s.items) - 1; i >= 0; i-- {
		switch v := s.items[i].Data.(type) {
		case sessionstore.Fork:
			return read, previousTail{}
		case session.Turn:
			if v.ContextStrategy != "" && v.ContextStrategy != contextengine.WindowThreeLayer {
				return read, previousTail{}
			}
		case sessionstore.HostRecord:
			if c := v.Context; c != nil {
				switch c.Kind {
				case sessionstore.ContextWindowStart, sessionstore.ContextWindowFallback, "summary", "header":
					return read, previousTail{}
				case sessionstore.ContextWindowBoundary:
					if c.Window != nil && c.Window.Tail != nil {
						return read, previousTail{c.Window.Tail.Previous}
					}
					return read, previousTail{}
				case "provider_conversation":
					if c.Conversation != nil && c.Conversation.Compacted {
						return read, previousTail{}
					}
				}
			}
		}
	}
	return read, previousTail{}
}

func (p previousTail) hits(locators []record.Locator) *int64 {
	if p.locators == nil {
		return nil
	}
	remaining := make(map[record.Locator]bool, len(*p.locators))
	for _, loc := range *p.locators {
		remaining[loc] = true
	}
	var hits int64
	for _, loc := range locators {
		if a, err := loc.Address(); err == nil {
			a.Line, a.Range = 0, nil
			base := a.Locator()
			if remaining[base] {
				hits++
				delete(remaining, base)
			}
		}
	}
	return &hits
}

func (s *Session) recordRead(ctx context.Context, read sessionstore.RecordRead) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	r := sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext,
		Context: &sessionstore.ContextRecord{Kind: "record_read", RecordRead: &read}}
	if err := s.store.AppendHostRecord(context.WithoutCancel(ctx), s.ID, r); err != nil {
		slog.Warn("record evidence read measurement", "action", read.Action)
	}
}

// importContext appends another Session's state as this Session's events.
// Event IDs derive from the source IDs, so repeating an import is a no-op.
// Copied IDs gain the source prefix; locators still name the source Session.
func (s *Session) importContext(ctx context.Context, note contextstate.ImportNote) (contextstate.Result, error) {
	from := strings.TrimSpace(note.From)
	if from == "" || strings.ContainsAny(from, ":/ \t\r\n") || from == string(s.ID) {
		return contextstate.Result{}, fmt.Errorf("import needs another Session's name")
	}
	prefix := func(id string) string { return from + "/" + id }
	prefixAll := func(ids []string) []string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = prefix(id)
		}
		return out
	}
	type item struct {
		kind  contextstate.EventKind
		id    string
		value any
	}
	var items []item
	for _, r := range note.Rules {
		if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Excerpt) == "" {
			return contextstate.Result{}, fmt.Errorf("imported rule needs an ID and an excerpt")
		}
		items = append(items, item{contextstate.RuleRecorded, r.ID, contextstate.Rule{ID: prefix(r.ID), Excerpt: r.Excerpt, Locator: r.Locator}})
	}
	for _, d := range note.Decisions {
		if strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Decision) == "" {
			return contextstate.Result{}, fmt.Errorf("imported decision needs an ID and a text")
		}
		id := d.ID
		d.Evidence, d.ID, d.Plan = contextstate.Evidence{}, prefix(id), prefixAll(d.Plan)
		items = append(items, item{contextstate.DecisionRecorded, id, d})
	}
	for _, p := range note.Plan {
		if strings.TrimSpace(p.ID) == "" {
			return contextstate.Result{}, fmt.Errorf("imported plan step needs an ID")
		}
		id := p.ID
		p.Evidence, p.ID, p.DependsOn, p.Decisions = contextstate.Evidence{}, prefix(id), prefixAll(p.DependsOn), prefixAll(p.Decisions)
		items = append(items, item{contextstate.PlanChanged, id, p})
	}
	for _, q := range note.Questions {
		if strings.TrimSpace(q.ID) == "" {
			return contextstate.Result{}, fmt.Errorf("imported question needs an ID")
		}
		id := q.ID
		q.Evidence, q.ID = contextstate.Evidence{}, prefix(id)
		items = append(items, item{contextstate.QuestionChanged, id, contextstate.QuestionUpdate{Question: q, Text: q.FirstLine}})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return contextstate.Result{}, ErrStopped
	}
	var last record.Locator
	for _, it := range items {
		event, err := contextstate.NewEvent("import:"+from+":"+string(it.kind)+":"+it.id, it.kind, it.value)
		if err != nil {
			return contextstate.Result{}, err
		}
		if last, err = s.store.AppendContextEvent(ctx, s.ID, event); err != nil {
			return contextstate.Result{}, err
		}
	}
	return contextstate.Result{ID: from, Locator: last}, nil
}

func noteID(explicit, fallback string) string {
	if explicit == "" {
		return fallback
	}
	return explicit
}

func (s *Session) noteSource(ctx context.Context, loc record.Locator, kind record.Kind) ([]byte, error) {
	addr, err := loc.Address()
	if err != nil || addr.Kind != kind || addr.Session != string(s.ID) {
		return nil, fmt.Errorf("source must reference a %s record in this Session", kind)
	}
	page, err := s.RecordShow(ctx, loc, 0)
	return page.Bytes, err
}

func (s *Session) recordContextNote(ctx context.Context, callID, id string, kind contextstate.EventKind, v any) (contextstate.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordContextNoteLocked(ctx, callID, id, kind, v)
}

func (s *Session) recordContextNoteLocked(ctx context.Context, callID, id string, kind contextstate.EventKind, v any) (contextstate.Result, error) {
	if !s.running {
		return contextstate.Result{}, ErrStopped
	}
	event, err := contextstate.NewEvent("note:"+callID, kind, v)
	if err != nil {
		return contextstate.Result{}, err
	}
	loc, err := s.store.AppendContextEvent(ctx, s.ID, event)
	return contextstate.Result{ID: id, Locator: loc}, err
}

func (s *Session) setContextPlan(ctx context.Context, callID string, note contextstate.PlanNote) (contextstate.Result, error) {
	var result contextstate.Result
	if strings.TrimSpace(note.ID) == "" {
		return result, fmt.Errorf("plan step ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return result, ErrStopped
	}
	if loc, err := s.store.ContextEventLocator(ctx, s.ID, "note:"+callID); err != nil {
		return result, err
	} else if loc != "" {
		return contextstate.Result{ID: note.ID, Locator: loc}, nil
	}
	state, err := s.store.ContextState(ctx, s.ID)
	if err != nil {
		return result, err
	}
	next := contextstate.PlanStep{ID: note.ID, Status: contextstate.PlanTodo, Owner: "self"}
	if i := slices.IndexFunc(state.Plan.Steps, func(v contextstate.PlanStep) bool { return v.ID == note.ID }); i >= 0 {
		next = state.Plan.Steps[i]
	} else if n := len(state.Plan.Steps); n != 0 {
		next.Position = state.Plan.Steps[n-1].Position + 1
	}
	if note.Position != nil {
		next.Position = *note.Position
	}
	if note.Text != nil {
		next.Text = *note.Text
	}
	if note.Owner != nil {
		next.Owner = *note.Owner
	}
	switch note.Status {
	case "":
	case contextstate.PlanTodo, contextstate.PlanDoing, contextstate.PlanDone, contextstate.PlanDropped:
		next.Status = note.Status
	default:
		return result, fmt.Errorf("unknown plan status %q", note.Status)
	}
	if note.DependsOn != nil {
		next.DependsOn = note.DependsOn
	}
	if note.Decisions != nil {
		next.Decisions = note.Decisions
	}
	if strings.TrimSpace(next.Text) == "" && next.Status != contextstate.PlanDone && next.Status != contextstate.PlanDropped {
		return result, fmt.Errorf("new plan step requires text")
	}
	return s.recordContextNoteLocked(ctx, callID, note.ID, contextstate.PlanChanged, next)
}

func (s *Session) answerContextQuestion(ctx context.Context, callID string, note contextstate.AnswerNote) (contextstate.Result, error) {
	var result contextstate.Result
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return result, ErrStopped
	}
	if loc, err := s.store.ContextEventLocator(ctx, s.ID, "note:"+callID); err != nil {
		return result, err
	} else if loc != "" {
		return contextstate.Result{ID: note.ID, Locator: loc}, nil
	}
	state, err := s.store.ContextState(ctx, s.ID)
	if err != nil {
		return result, err
	}
	question, ok := state.QuestionsToUser.Current[note.ID]
	if !ok {
		return result, fmt.Errorf("open question %q not found", note.ID)
	}
	question.Status, question.Answer = contextstate.QuestionAnswered, note.Source
	return s.recordContextNoteLocked(ctx, callID, note.ID, contextstate.QuestionChanged, question)
}
