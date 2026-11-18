package contextstate

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

func fixtureEvent(t *testing.T, n int, kind EventKind, v any) Event {
	t.Helper()
	e, err := NewEvent(fmt.Sprint(n), kind, v)
	if err != nil {
		t.Fatal(err)
	}
	e.Record = record.Item("events", uint64(n))
	return e
}

func reduceEvents(t *testing.T, events []Event) Projection {
	t.Helper()
	var p Projection
	for _, event := range events {
		next, err := Reduce(p, event)
		if err != nil {
			t.Fatal(err)
		}
		p = next
	}
	return p
}

func stateBytes(t *testing.T, state State) []byte {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReducerIndependentOrderAndIdempotence(t *testing.T) {
	const source = record.Locator("rec:events/100")
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	events := []Event{
		fixtureEvent(t, 1, SessionChanged, Session{ID: "events", Started: at}),
		fixtureEvent(t, 2, RequestChanged, Request{ID: "work", Origin: HumanInput, Status: RequestActive, Locator: source, Text: "whole request\n原文"}),
		fixtureEvent(t, 3, RuleRecorded, Rule{ID: "rule", Excerpt: "user's rule", Locator: source}),
		fixtureEvent(t, 4, PlanChanged, PlanStep{ID: "second", Position: 2, Text: "run test", Status: PlanTodo, DependsOn: []string{"first"}}),
		fixtureEvent(t, 5, PlanChanged, PlanStep{ID: "first", Position: 1, Text: "edit", Status: PlanDoing, Owner: "self"}),
		fixtureEvent(t, 6, DecisionRecorded, Decision{ID: "choice", Decision: "preserve the original", Basis: []record.Locator{source}}),
		fixtureEvent(t, 7, ChildChanged, ChildUpdate{Child: Child{Handle: "child", State: ChildRunning, Template: "reader"}}),
		fixtureEvent(t, 8, ApprovalChanged, Approval{ID: "approval", Request: "approval request", Locator: source}),
		fixtureEvent(t, 9, WaitChanged, WaitUpdate{Wait: Wait{ID: "service", Kind: WaitExternal, Text: "wait for service"}}),
		fixtureEvent(t, 10, QuestionChanged, Question{ID: "ask", FirstLine: "first question", Locator: source, Status: QuestionOpen}),
		fixtureEvent(t, 11, ArtifactChanged, Artifact{Path: "/workspace/file.go", LastWriter: "self", Locator: source, TS: at}),
		fixtureEvent(t, 12, IssueChanged, Issue{ID: "issue", Text: "investigate issue", Status: IssueOpen, Locator: source}),
		fixtureEvent(t, 13, RecentChanged, []RecentEvent{{TS: at, Kind: "input", FirstLine: "event", Locator: source}}),
		fixtureEvent(t, 14, MetricsChanged, Metrics{ChildrenByState: map[ChildStatus]uint64{ChildRunning: 1, ChildFinished: 0}}),
	}
	want := reduceEvents(t, events)
	encoded := stateBytes(t, want.State)
	if want.State.Plan.Steps[0].ID != "first" || len(want.State.Waiting.Current) != 3 ||
		want.State.Requests.Current["work"].Text != "whole request\n原文" || len(want.State.RecentEvents) != 1 {
		t.Fatalf("missing current state: %s", encoded)
	}
	for seed := range uint64(48) {
		order := rand.New(rand.NewPCG(seed, seed+1)).Perm(len(events))
		var p Projection
		for _, i := range order {
			snapshot := stateBytes(t, p.State)
			next, err := Reduce(p, events[i])
			if err != nil || !bytes.Equal(snapshot, stateBytes(t, p.State)) {
				t.Fatal("reducer changed its input", err)
			}
			p = next
		}
		for i := range events {
			next, err := Reduce(p, events[i])
			if err != nil || len(next.Applied) != len(events) || !bytes.Equal(stateBytes(t, next.State), encoded) {
				t.Fatalf("seed %d, repeated event %s changed state: %v", seed, events[i].ID, err)
			}
			p = next
		}
	}
}

func TestReducerKeepsOnlyProcessedCountsAndLocators(t *testing.T) {
	const old = record.Locator("rec:events/90")
	const other = record.Locator("rec:events/91")
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	long := strings.Repeat("日本語", 1000)
	events := []Event{
		fixtureEvent(t, 1, RequestChanged, Request{ID: "long", Origin: HumanInput, Status: RequestActive, Locator: old, Text: long + "\r\nkeep the whole request"}),
		fixtureEvent(t, 2, RequestChanged, Request{ID: "done", Origin: HumanInput, Status: RequestActive, Text: "processed request", Locator: old}),
		fixtureEvent(t, 3, RequestChanged, Request{ID: "alias", Origin: MixedInput, Status: RequestActive, Locator: other, Original: old, Text: "mixed peer body"}),
		fixtureEvent(t, 4, PlanChanged, PlanStep{ID: "test", Text: "processed plan", Status: PlanDoing}),
		fixtureEvent(t, 5, ChildChanged, ChildUpdate{Child: Child{Handle: "reader", State: ChildFinished, Template: "collector", Model: "fixture", EndedBy: EndedByFinish, Title: "processed child", Report: old}}),
		fixtureEvent(t, 6, ChildChanged, ChildUpdate{Child: Child{Handle: "review", State: ChildFailed, Title: "needs acceptance", Report: other}}),
		fixtureEvent(t, 7, ApprovalChanged, Approval{ID: "permit", Request: "processed approval", Locator: old}),
		fixtureEvent(t, 8, QuestionChanged, Question{ID: "ask", Status: QuestionOpen, FirstLine: "processed question", Locator: old}),
		fixtureEvent(t, 9, IssueChanged, Issue{ID: "issue", Status: IssueOpen, Text: "processed issue", Locator: old}),
		fixtureEvent(t, 10, ArtifactChanged, Artifact{Path: "/processed child artifact", LastWriter: "reader", TS: at, Locator: old}),
		fixtureEvent(t, 11, ArtifactChanged, Artifact{Path: "/processed earlier artifact", LastWriter: "self", TS: at, Locator: old}),
		fixtureEvent(t, 12, ArtifactChanged, Artifact{Path: "/current artifact", LastWriter: "self", TS: at.Add(time.Second), Locator: other}),
	}
	prefix := reduceEvents(t, events)
	longRequest := prefix.State.Requests.Current["long"]
	if longRequest.FirstLine != long || longRequest.Text != long+"\r\nkeep the whole request" || longRequest.Chars != len([]rune(longRequest.Text)) {
		t.Fatal("first line, text or character count changed")
	}
	if got := prefix.State.Requests.Current["alias"]; got.Text != "" || got.Locator != other || got.Original != old {
		t.Fatalf("mixed alias was attributed to the human: %+v", got)
	}
	if _, ok := prefix.State.Children.Current["reader"]; !ok {
		t.Fatal("unreviewed Finish should stay current")
	}
	events = append(events,
		fixtureEvent(t, 13, RequestChanged, Request{ID: "done", Status: RequestDone}),
		fixtureEvent(t, 14, PlanChanged, PlanStep{ID: "test", Status: PlanDropped}),
		fixtureEvent(t, 15, ChildChanged, ChildUpdate{Child: Child{Handle: "reader", Accept: AcceptRejected}, Why: "explicit review reason"}),
		fixtureEvent(t, 16, ApprovalChanged, Approval{ID: "permit", Decision: "allow"}),
		fixtureEvent(t, 17, QuestionChanged, Question{ID: "ask", Status: QuestionAnswered, Answer: other}),
		fixtureEvent(t, 18, IssueChanged, Issue{ID: "issue", Status: IssueClosed}),
		fixtureEvent(t, 19, MetricsChanged, Metrics{LastBoundary: &Boundary{TS: at.Add(time.Second), Locator: other}}),
	)
	s := reduceEvents(t, events).State
	if got := s.Children.ByEnding; len(got) != 1 || got[0] != (ChildCount{"collector", "fixture", EndedByFinish, 1}) {
		t.Fatalf("child grouping: %+v", got)
	}
	for _, tc := range []struct {
		value Processed
		count uint64
	}{{s.Requests.Processed, 1}, {s.Plan.Processed, 1}, {s.Children.Processed, 1}, {s.Approvals.Processed, 1},
		{s.Waiting.Processed, 2}, {s.QuestionsToUser.Processed, 1}, {s.Artifacts.Processed, 2}, {s.Issues.Processed, 1}} {
		if tc.value.Count != tc.count || len(tc.value.Sources) == 0 {
			t.Fatalf("expected %d processed records with sources, got %+v", tc.count, tc.value)
		}
	}
	if _, ok := s.QuestionsToUser.Processed.Sources[other]; !ok {
		t.Fatal("processed question lost the explicit answer")
	}
	if len(s.Artifacts.Current) != 1 || s.Artifacts.Current["/current artifact"].Locator != other || len(s.Waiting.Current) != 0 ||
		len(s.Children.Current) != 1 || s.Children.Current["review"].Accept != AcceptPending {
		t.Fatalf("current entries changed: %s", stateBytes(t, s))
	}
	for _, forbidden := range []string{"processed request", "processed plan", "processed child", "processed approval",
		"processed question", "processed issue", "processed earlier artifact", "explicit review reason", "mixed peer body"} {
		if bytes.Contains(stateBytes(t, s), []byte(forbidden)) {
			t.Fatalf("L2 still contains %q", forbidden)
		}
	}
}

func TestContextStateSchemaDecodesAdditiveChanges(t *testing.T) {
	var state State
	if err := json.Unmarshal([]byte(`{"version":2,"session":{"id":"existing","unknown_session_field":true},"future_slot":{"ok":true}}`), &state); err != nil {
		t.Fatal(err)
	}
	if encoded := stateBytes(t, state); state.Session.ID != "existing" || state.Version != 2 || !bytes.Contains(encoded, []byte(`"future_slot":{"ok":true}`)) {
		t.Fatalf("new state fields lost on decode: %s", encoded)
	}
	var event Event
	const future = `{"version":2,"id":"future","kind":"rule","opaque":{"id":2},"data":{"new_shape":true}}`
	if err := json.Unmarshal([]byte(future), &event); err != nil {
		t.Fatal(err)
	}
	recorded, err := json.Marshal(event)
	if err != nil || !bytes.Contains(recorded, []byte(`"opaque":{"id":2}`)) || !bytes.Contains(recorded, []byte(`"new_shape":true`)) {
		t.Fatalf("future event was lost: %s (%v)", recorded, err)
	}
	p, err := Reduce(Projection{}, event)
	if err != nil || len(p.State.Rules) != 0 || len(p.Applied) != 1 {
		t.Fatalf("future event changed a known slot: %+v (%v)", p, err)
	}
	event.Version, event.Kind, event.ID = Version, "later_event", "new-kind"
	p, err = Reduce(p, event)
	if err != nil || len(p.State.Rules) != 0 || len(p.Applied) != 2 {
		t.Fatalf("unknown kind affected known slots: %+v (%v)", p, err)
	}
	if err := json.Unmarshal([]byte(`{"session":{"id":"unversioned"}}`), &state); err == nil {
		t.Fatal("unversioned state accepted")
	}
}
