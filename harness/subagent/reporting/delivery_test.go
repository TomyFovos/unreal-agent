package reporting

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func reportOperation(t *testing.T, report string) operation.Operation {
	t.Helper()
	value, err := json.Marshal(observed.Result{Tool: "Bash", Content: report})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := operation.NewValueSpec(value)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: "read", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusCompleted}
}

func TestLegacyBatchIndexesEveryEndWithoutMatchingMentions(t *testing.T) {
	payload, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "first-child", Target: "parent", Handle: "first", Kind: "ended", Text: "Child first ended: completed\nSummary: second-other is still running\n(Full report: subagent show first)\n\nChild second ended: failed\nError: fixture\n(Full report: subagent show second)\n\nChild third ended: canceled\n(Full report: subagent show third)\nChild mentioned ended: completed\nmissing footer"})
	if err != nil {
		t.Fatal(err)
	}
	var index Index
	for range 2 {
		index.ObserveItem(sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "ended:first", Kind: inbox.InputPeer, Payload: payload}})
	}
	for _, id := range []operation.ID{"first", "second", "third"} {
		if !index.Has(id) {
			t.Fatal("lost batched report", id)
		}
	}
	for _, id := range []operation.ID{"second-other", "mentioned"} {
		if index.Has(id) {
			t.Fatal("mistook a mention for delivery", id)
		}
	}
}

func TestInlineFinishDoesNotIndexHeadingsInsideTheReport(t *testing.T) {
	payload, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "first-child", Target: "parent", Handle: "first", Kind: "ended", ReportIncluded: true,
		Text: "Child first ended: completed\nSummary: Example logs follow\nChild second ended: completed\n(Full report: subagent show second)"})
	if err != nil {
		t.Fatal(err)
	}
	var index Index
	index.ObserveItem(sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "ended:first", Kind: inbox.InputPeer, Payload: payload}})
	if !index.Has("first") || index.Has("second") {
		t.Fatal("inline report classified another child's example heading as delivery")
	}
}

func TestCLIReportsRequireAnEndForTheExactHandle(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"show", "== child  worker  completed\nresult: completed\n", true},
		{"wait", "== other  worker  awaiting\n== child  worker  failed\nresult: failed\n", true},
		{"canceled_without_finish", "== child  worker  canceled\n", true},
		{"running", "== child  worker  awaiting\ntask: still working\n", false},
		{"other_handle", "== child-other  worker  completed\nresult: completed\n", false},
		{"listing", "child  worker  completed\n", false},
		{"mention", "report for child\nresult: completed\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var index Index
			index.ObserveOperation(reportOperation(t, test.text))
			if index.Has("child") != test.want {
				t.Fatal("delivery", index.Has("child"), "want", test.want)
			}
		})
	}
}

func TestTruncatedShowMustMatchCanonicalFinishInEitherReplayOrder(t *testing.T) {
	result := sessionstore.FinishResult{Status: "completed", Summary: "child finished", ChangedFiles: []string{"fixture.go"}, Tests: []string{"focused: PASS"}}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start"}`)})
	if err != nil {
		t.Fatal(err)
	}
	start := operation.Operation{ID: "child", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusCompleted, MaxOutputLength: spec.MaxOutputLength}
	state, _ := operation.DecodeRemoteJobState(start)
	state.Handle, _ = json.Marshal(struct{ Finish *sessionstore.FinishRecord }{&sessionstore.FinishRecord{Version: 1, OperationID: "finish", Result: result}})
	step, err := operation.UpdateRemoteJob(start, state, operation.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	start = *step.Operation
	for _, reversed := range []bool{false, true} {
		for _, matches := range []bool{false, true} {
			var index Index
			tail := "changed files:\n  - fixture.go\ntests:\n  - focused: PASS\nblockers: none\n"
			if !matches {
				tail = "changed files:\n  - different.go\ntests:\n  - focused: PASS\nblockers: none\n"
			}
			read := reportOperation(t, "=== child\nlast part of summary\n"+tail+"=== other\nstill running\n")
			ops := []operation.Operation{start, read}
			if reversed {
				ops = []operation.Operation{read, start}
			}
			index.ObserveOperation(ops[0])
			if index.Has("child") {
				t.Fatal("one side of the join established delivery")
			}
			index.ObserveOperation(ops[1])
			if index.Has("child") != matches {
				t.Fatal("truncated report delivery", reversed, matches, index.Has("child"))
			}
		}
	}
}
