package contextstate

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

func TestRenderRecentEventsFromRequestOrBoundary(t *testing.T) {
	request := record.Input("events", 10, record.UserMessage)
	state := State{
		Requests: Entries[Request]{Current: map[string]Request{
			"request": {Origin: HumanInput, Status: RequestActive, Locator: request},
			"parked":  {Origin: HumanInput, Status: RequestParked, Locator: record.Input("events", 30, record.UserMessage)},
		}},
		RecentEvents: []RecentEvent{
			{Kind: "old", After: 9},
			{Kind: "active input", Locator: request},
			{Kind: "before the boundary", After: 18},
			{Kind: "checkpoint after boundary", After: 20}, // Legacy operation timestamps can be absent.
			{Kind: "newer item", Locator: record.Item("events", 25)},
		},
	}
	check := func(got State, want ...string) {
		t.Helper()
		var kinds []string
		for _, event := range got.RecentEvents {
			kinds = append(kinds, event.Kind)
		}
		if !slices.Equal(kinds, want) {
			t.Fatalf("recent events = %v, want %v", kinds, want)
		}
	}
	check(state.Render(0), "active input", "before the boundary", "checkpoint after boundary", "newer item")
	check(state.Render(5), "old", "active input", "before the boundary", "checkpoint after boundary", "newer item")
	state.Metrics.LastBoundary = &Boundary{Locator: record.Item("events", 20)}
	check(state.Render(0), "checkpoint after boundary", "newer item")
	check(state.Render(1), "newer item")
	state.Requests.Current, state.Metrics.LastBoundary = nil, nil
	check(state.Render(0))
}

func TestRenderCurrentArtifactsQuestionsAndWaits(t *testing.T) {
	const child = "worker"
	at := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	var p Projection
	apply := func(seq int, kind EventKind, v any) {
		t.Helper()
		if err := p.Apply(fixtureEvent(t, seq, kind, v)); err != nil {
			t.Fatal(err)
		}
	}
	write := func(seq int, path, writer string) {
		apply(seq, ArtifactChanged, Artifact{Path: path, LastWriter: writer, After: uint64(seq), TS: at})
	}
	marker := func(seq int, id string) {
		loc := record.Address{Session: "events", Kind: record.AssistantMessage, Sequence: uint64(seq), Line: 1}.Locator()
		apply(seq, QuestionChanged, Question{ID: id, Marker: true, FirstLine: "【要確認】 fixture", Status: QuestionOpen, Locator: loc})
	}
	apply(1, SessionChanged, Session{ID: "events"})
	apply(2, ChildChanged, ChildUpdate{Child: Child{Handle: child, State: ChildRunning}})
	write(3, "old-self", "self")
	write(4, "shared-child-path", child)
	marker(5, "preceded-input")
	apply(6, BoundaryObserved, Boundary{Locator: record.Item("events", 6), TS: at})
	write(7, "shared-child-path", "past-worker")
	write(8, "new-self", "self")
	write(9, "new-self", "past-worker")
	write(10, "retired", "past-worker")
	write(11, "retired", "past-worker")
	marker(12, "answered")
	apply(13, RequestChanged, Request{ID: "human", Origin: HumanInput, Locator: record.Item("events", 13)})
	apply(14, QuestionChanged, Question{ID: "answered", Status: QuestionAnswered, Answer: record.Input("events", 13, record.UserMessage)})
	apply(15, QuestionChanged, Question{ID: "note", Status: QuestionOpen, FirstLine: "A private note"})
	marker(16, "open")
	apply(17, ApprovalChanged, Approval{ID: "child-approval", Handle: child, Request: "allow tool"})
	apply(18, ApprovalChanged, Approval{ID: "parent-approval", Request: "initial request"})
	apply(19, ApprovalChanged, Approval{ID: "parent-approval", Request: "replacement request"})
	apply(20, WaitChanged, WaitUpdate{Wait: Wait{ID: "external", Kind: WaitExternal, Text: "old wait"}})
	apply(21, WaitChanged, WaitUpdate{Wait: Wait{ID: "external", Kind: WaitExternal, Text: "current wait"}})
	apply(22, WaitChanged, WaitUpdate{Wait: Wait{ID: "child-external", Kind: WaitExternal, Handle: child}})
	apply(23, IssueChanged, Issue{ID: "open-issue", Status: IssueOpen})
	apply(24, IssueChanged, Issue{ID: "closed-issue", Status: IssueClosed})
	before := p.State.Render(0)
	paths := before.Artifacts.Current
	if len(paths) != 2 || paths["shared-child-path"].LastWriter != "past-worker" || paths["new-self"].LastWriter != "past-worker" ||
		before.Artifacts.Processed.Count != 2 || before.Artifacts.Processed.Locator != "rec:events/context/artifacts" {
		t.Fatalf("S10 omitted shared paths or included a historical path: %+v", before.Artifacts)
	}

	apply(25, ChildChanged, ChildUpdate{Child: Child{Handle: child, State: ChildFinished}, CancelRecorded: true})
	apply(26, ApprovalChanged, Approval{ID: "late-approval", Handle: child, Request: "late"})
	apply(27, WaitChanged, WaitUpdate{Wait: Wait{ID: "late-wait", Kind: WaitExternal, Handle: child}})
	apply(28, WaitChanged, WaitUpdate{Wait: Wait{ID: "external", Kind: WaitExternal}, Resolved: true})
	state := p.State.Render(0)
	if len(state.Artifacts.Current) != 1 || state.Artifacts.Current["new-self"].Path == "" || state.Artifacts.Processed.Count != 3 {
		t.Fatalf("settled child still owns a path: %+v", state.Artifacts)
	}
	if len(state.QuestionsToUser.Current) != 1 || state.QuestionsToUser.Current["open"].ID == "" ||
		state.QuestionsToUser.Processed.Count != 3 || state.QuestionsToUser.Processed.Locator == "" {
		t.Fatalf("closed, superseded, and unmarked questions leaked into S9: %+v", state.QuestionsToUser)
	}
	if len(state.Approvals.Current) != 1 || state.Approvals.Current["parent-approval"].Request != "replacement request" ||
		state.Approvals.Processed.Count != 3 {
		t.Fatalf("only the latest open approval should survive: %+v", state.Approvals)
	}
	if len(state.Waiting.Current) != 2 || state.Waiting.Current["user_answer:open"].ID == "" ||
		state.Waiting.Current["approval:parent-approval"].ID == "" || state.Waiting.Processed.Count != 9 {
		t.Fatalf("only open waits should survive: %+v", state.Waiting)
	}
	if len(state.Issues.Current) != 1 || state.Issues.Current["open-issue"].ID == "" || state.Issues.Processed.Count != 1 {
		t.Fatalf("open/closed issues: %+v", state.Issues)
	}
	if len(p.State.QuestionsToUser.Current) != 3 || len(state.Waiting.Processed.Sources) != 0 {
		t.Fatal("render mutated the replay state or copied historical sources into L2")
	}
}

func TestRenderLargeSessionWithUnreviewedChildren(t *testing.T) {
	const children = 1_200
	var p Projection
	seq := 0
	apply := func(kind EventKind, value any) {
		t.Helper()
		seq++
		if err := p.Apply(fixtureEvent(t, seq, kind, value)); err != nil {
			t.Fatal(err)
		}
	}
	setChild := func(handle string, status ChildStatus) {
		apply(ChildChanged, ChildUpdate{Child: Child{
			Handle: handle, Template: "worker", Model: "fixture", State: status, EndedAfter: uint64(seq + 1),
			EndedBy: EndedByFinish, Title: "Historical report with a long brief", Purpose: strings.Repeat("work ", 128),
		}})
		apply(ArtifactChanged, Artifact{Path: "path-" + handle, LastWriter: handle})
	}
	apply(SessionChanged, Session{ID: "events"})
	for i := range children {
		handle := fmt.Sprintf("finished-%d", i)
		setChild(handle, ChildFinished)
		apply(ChildReportSeen, handle)
		apply(RecentObserved, RecentEvent{Kind: "child reported", After: uint64(seq + 1), FirstLine: strings.Repeat("historical ", 128)})
	}
	setChild("not-delivered", ChildFinished)
	apply(RequestChanged, Request{
		ID: "latest", Origin: HumanInput, Status: RequestActive, Locator: record.Input("events", uint64(seq+1), record.UserMessage), Text: "active task",
	})
	setChild("pending-review", ChildFinished)
	apply(ChildReportSeen, "pending-review")
	setChild("ended-after-response", ChildFinished)
	setChild("running", ChildRunning)
	s := p.State.Render(0)
	for _, handle := range []string{"not-delivered", "pending-review", "ended-after-response", "running"} {
		if s.Children.Current[handle].Handle != handle || s.Artifacts.Current["path-"+handle].Path == "" {
			t.Fatalf("child %q or its artifact was omitted", handle)
		}
	}
	if len(s.Children.Current) != 4 || len(s.Artifacts.Current) != 4 ||
		s.Children.Processed.Count != children || s.Artifacts.Processed.Count != children ||
		!slices.Equal(s.Children.ByEnding, []ChildCount{{"worker", "fixture", EndedByFinish, children}}) ||
		s.Children.Processed.Locator != "rec:events/context/children" {
		t.Fatalf("processed children or paths were not grouped: %+v", s.Children.Processed)
	}
	if data := stateBytes(t, s); len(data) >= 12_000 || len(s.RecentEvents) != 0 {
		t.Fatalf("bounded current state contains %d bytes and %d recent events", len(data), len(s.RecentEvents))
	}
}
