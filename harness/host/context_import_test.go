package host

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestStateImportCopiesAnotherSessionsStateOnce(t *testing.T) {
	ctx := t.Context()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src, err := localfile.New(srcDir)
	checkHostRestore(t, err)
	_, err = src.Create(ctx, "ci")
	checkHostRestore(t, err)
	rule := record.Item("ci", 7)
	for _, e := range []struct {
		kind  contextstate.EventKind
		value any
	}{
		{contextstate.RuleRecorded, contextstate.Rule{ID: "r1", Excerpt: "max four tests", Locator: rule}},
		{contextstate.DecisionRecorded, contextstate.Decision{ID: "d1", Decision: "merge #474", Plan: []string{"p1"}, Locator: record.Item("ci", 9)}},
		{contextstate.PlanChanged, contextstate.PlanStep{ID: "p1", Position: 1, Text: "Windows check", Status: contextstate.PlanTodo, DependsOn: []string{"p0"}}},
		{contextstate.PlanChanged, contextstate.PlanStep{ID: "p2", Position: 2, Text: "finished", Status: contextstate.PlanDone}},
		{contextstate.QuestionChanged, contextstate.Question{ID: "q1", FirstLine: "【要確認】D1?", Status: contextstate.QuestionOpen, Locator: record.Item("ci", 11)}},
	} {
		event, err := contextstate.NewEvent(string(e.kind)+"-seed", e.kind, e.value)
		checkHostRestore(t, err)
		_, err = src.AppendContextEvent(ctx, "ci", event)
		checkHostRestore(t, err)
	}
	read, err := localfile.OpenExisting(srcDir)
	checkHostRestore(t, err)
	state, err := read.ContextState(ctx, "ci")
	checkHostRestore(t, err)
	note := contextstate.ImportFrom("ci", state)
	if len(note.Plan) != 1 || len(note.Questions) != 1 || len(note.Rules) != 1 || len(note.Decisions) != 1 {
		t.Fatalf("import selected the wrong state: %+v", note)
	}

	dst, err := localfile.New(dstDir)
	checkHostRestore(t, err)
	_, err = dst.Create(ctx, "main")
	checkHostRestore(t, err)
	h := newTestHost(t, dstDir)
	defer h.Close()
	s, err := h.Resume(ctx, Options{ID: "main", Policy: permission.Unrestricted()})
	checkHostRestore(t, err)
	raw, err := json.Marshal(note)
	checkHostRestore(t, err)
	importOnce := func(input string) {
		t.Helper()
		if _, err := s.ContextCommand(ctx, s.Generation, input, contextstate.Command{Action: contextstate.Import, Args: raw}); err != nil {
			t.Fatal(err)
		}
	}
	importOnce("import-1")
	before, err := s.ContextState(ctx)
	checkHostRestore(t, err)
	importOnce("import-2")
	after, err := s.ContextState(ctx)
	checkHostRestore(t, err)
	a, b := mustJSON(t, before), mustJSON(t, after)
	if a != b {
		t.Fatal("repeating an import changed the state")
	}
	if r := after.Rules["ci/r1"]; r.Excerpt != "max four tests" || r.Locator != rule {
		t.Fatalf("imported rule lost its quote or source: %+v", r)
	}
	if d := after.Decisions["ci/d1"]; d.Decision != "merge #474" || len(d.Plan) != 1 || d.Plan[0] != "ci/p1" || d.Locator != record.Item("ci", 9) {
		t.Fatalf("imported decision changed: %+v", d)
	}
	if len(after.Plan.Steps) != 1 || after.Plan.Steps[0].ID != "ci/p1" || after.Plan.Steps[0].DependsOn[0] != "ci/p0" {
		t.Fatalf("imported plan changed: %+v", after.Plan.Steps)
	}
	if q, ok := after.QuestionsToUser.Current["ci/q1"]; !ok || q.Status != contextstate.QuestionOpen {
		t.Fatalf("open question was not imported: %+v", after.QuestionsToUser)
	}
	for _, bad := range []string{"", "main", "a/b"} {
		raw, _ := json.Marshal(contextstate.ImportNote{From: bad})
		if _, err := s.ContextCommand(ctx, s.Generation, "bad-"+bad, contextstate.Command{Action: contextstate.Import, Args: raw}); err == nil {
			t.Fatalf("import from %q was accepted", bad)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	checkHostRestore(t, err)
	return string(raw)
}
