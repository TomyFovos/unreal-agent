package localfile

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
)

func TestContextMachineFactsReplay(t *testing.T) {
	ctx := t.Context()
	store, id, _ := newRestoreFixture(t)
	state := func() contextstate.State {
		v, err := store.ContextState(ctx, id)
		mustCheckpoint(t, err)
		return v
	}
	input := func(in inbox.Input, kind record.Kind) record.Locator {
		seq, err := store.AppendInputRecorded(ctx, id, in)
		mustCheckpoint(t, err)
		return record.Input(string(id), uint64(seq), kind)
	}
	hostRecord := func(r sessionstore.HostRecord) {
		r.Version = 1
		mustCheckpoint(t, store.AppendHostRecord(ctx, id, r))
	}
	activity := func(a llm.ActivityRecord) {
		hostRecord(sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &a})
	}
	putOperation := func(op operation.Operation, job operation.RemoteJobState, status operation.Status) record.Locator {
		step, err := operation.UpdateRemoteJob(op, job, status)
		mustCheckpoint(t, err)
		loc, err := store.SaveOperationRecorded(ctx, id, *step.Operation)
		mustCheckpoint(t, err)
		return loc
	}
	const handle = "child-op"
	childID := identity.ChildID(id, handle)
	peer := func(key, kind, text, rule string) record.Locator {
		raw, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(childID), Target: string(id), Handle: handle, Kind: kind, Text: text, RuleID: rule})
		mustCheckpoint(t, err)
		return input(inbox.Input{ID: inbox.ID(key), Kind: inbox.InputPeer, Payload: raw}, record.PeerMessage)
	}
	long := strings.Repeat("原文", 3500)
	user := input(inbox.Input{ID: "human", Kind: inbox.InputExternal, Payload: mustContextJSON(t, long+"\r\nverbatim")}, record.UserMessage)
	if got := state().Requests.Current["human"]; got.FirstLine != long || got.Chars != len([]rune(long+"\r\nverbatim")) || got.Locator != user || got.Origin != contextstate.HumanInput || got.Text != "" {
		t.Fatalf("human request: %+v", got)
	}
	mustCheckpoint(t, store.AppendTurn(ctx, id, session.Turn{ID: "turn"}))
	mustCheckpoint(t, store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Normal text.\n【要確認】 Choose the fixture?\nOptions."}},
	}}}))
	current := state()
	if len(current.QuestionsToUser.Current) != 1 || len(current.Waiting.Current) != 1 {
		t.Fatalf("question or its wait missing: %+v", current)
	}
	for _, q := range current.QuestionsToUser.Current {
		addr, err := q.Locator.Address()
		mustCheckpoint(t, err)
		if q.Status != contextstate.QuestionOpen || q.FirstLine != "【要確認】 Choose the fixture?" || addr.Kind != record.AssistantMessage || addr.Line != 2 {
			t.Fatalf("question: %+v", q)
		}
	}
	answer := input(inbox.Input{ID: "another-input", Kind: inbox.InputExternal, Payload: []byte(`"An unrelated message"`)}, record.UserMessage)
	if len(state().QuestionsToUser.Current) != 1 || state().Requests.Current["another-input"].Locator != answer {
		t.Fatal("external input implicitly closed the question")
	}

	plan := childPlan{Action: "start", ParentID: string(id), Template: "worker", Brief: childBrief{Title: "fixture title", Purpose: "fixture purpose", Outputs: "two paths", Verify: "test command", Acceptance: "exit zero"}}
	op, job := contextJob(t, store, id, handle, "SubagentStart", "subagent", plan)
	at := time.Now().UTC()
	activity(llm.ActivityRecord{Kind: "child_start", ID: handle, StartedAt: at, Model: "fixture-model", Effort: "high"})
	current = state()
	start := current.Children.Current[handle]
	if start.Title != plan.Brief.Title || start.Purpose != plan.Brief.Purpose || start.Outputs != plan.Brief.Outputs ||
		start.Verification != plan.Brief.Verify || start.AcceptanceCriteria != plan.Brief.Acceptance || !start.Started.Equal(at) ||
		start.Model != "fixture-model" || start.Effort != "high" || start.State != contextstate.ChildRunning || start.Accept != contextstate.AcceptPending {
		t.Fatalf("child start: %+v", start)
	}
	if _, waiting := current.Waiting.Current["child_completion:"+handle]; waiting {
		t.Fatal("an asynchronous child start is not a wait")
	}
	progress := peer("progress", "progress", long+"\nsecond line", "")
	next := state()
	if !reflect.DeepEqual(next.Children, current.Children) || !reflect.DeepEqual(next.Waiting, current.Waiting) {
		t.Fatal("progress changed state outside recent events")
	}
	if got := next.RecentEvents[len(next.RecentEvents)-1]; got.Locator != progress || got.FirstLine != long || got.RuleID != "P1" {
		t.Fatal("progress event lost its first line, route or locator")
	}
	q1 := peer("q1", "message", "brief unspecified_field", "X1")
	send, sendJob := contextJob(t, store, id, "send-op", "SubagentSend", "subagent", childPlan{Action: "send", ParentID: string(id), Handle: handle})
	if q := state().Children.Current[handle]; q.State != contextstate.ChildAsked || !slices.Equal(q.UnansweredQuestions, []record.Locator{q1}) {
		t.Fatalf("forwarded question: %+v", q)
	}
	retry := at.Add(time.Hour)
	activity(llm.ActivityRecord{Kind: "usage_limit_wait", RetryAt: retry})
	activity(llm.ActivityRecord{Kind: llm.ActivityModelFailure, ID: "turn", EndedAt: at, Reason: "recorded failure"})
	if got := state().Waiting.Current; !got["usage_limit_wait:model"].RetryAt.Equal(retry) || got["model_failure:parent"].Text != "recorded failure" {
		t.Fatalf("model waits: %+v", got)
	}
	checkpointWriteHead(t, store, id)
	q2 := peer("q2", "message", "A second question", "Q7")
	sendJob.TerminalResult = "Subagent send acknowledged"
	putOperation(send, sendJob, operation.StatusCompleted)
	if got := state().Children.Current[handle]; got.Sends != 1 || !slices.Equal(got.UnansweredQuestions, []record.Locator{q2}) {
		t.Fatalf("send answered the wrong question: %+v", got)
	}
	job.Handle = mustContextJSON(t, childHandle{Version: 1, Approvals: []childApproval{{ID: "request", Tool: "Bash", Scope: "fixture"}}})
	approval := putOperation(op, job, operation.StatusAwaiting)
	current = state()
	if a := current.Approvals.Current[handle+":request"]; a.Locator != approval || a.Request != "Bash fixture" ||
		current.Children.Current[handle].State != contextstate.ChildWaitingApproval || len(current.Waiting.Current["approval:"+handle+":request"].Sources) != 1 {
		t.Fatalf("child approval request: %+v", current.Approvals)
	}
	approve, approveJob := contextJob(t, store, id, "approve-op", "SubagentApprove", "subagent", childPlan{Action: "approve", ParentID: string(id), Handle: handle, Request: "request", Allow: true})
	approved := putOperation(approve, approveJob, operation.StatusCompleted)
	current = state()
	if len(current.Approvals.Current) != 0 || current.Approvals.Processed.Count != 1 {
		t.Fatalf("approval did not settle: %+v", current.Approvals)
	}
	for _, loc := range []record.Locator{approval, approved} {
		if _, exists := current.Approvals.Processed.Sources[loc]; !exists {
			t.Fatalf("processed approval lost %s", loc)
		}
	}

	finish := &sessionstore.FinishRecord{Version: 1, OperationID: "finish", Result: sessionstore.FinishResult{Status: "completed", Summary: "done\nfull report", ChangedFiles: []string{"deliverable.md"}}}
	_, err := store.CreateWithHostRecords(ctx, childID, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: finish})
	mustCheckpoint(t, err)
	report := record.Address{Session: string(childID), Kind: record.ChildReport, Sequence: 1, Handle: handle}
	job.Handle = mustContextJSON(t, childHandle{Version: 1, Finish: finish, FinishLocator: report.Locator()})
	end := putOperation(op, job, operation.StatusCompleted)
	report.Section = "changed-files"
	current = state()
	if got := current.Children.Current[handle]; got.State != contextstate.ChildFinished || got.EndedBy != contextstate.EndedByFinish ||
		got.ResultFirstLine != "done" || got.LastEvent != end || !slices.Contains(got.Sources, q2) || got.Accept != contextstate.AcceptPending {
		t.Fatalf("child completion: %+v", got)
	}
	if got := current.Artifacts.Current["deliverable.md"]; got.Locator != report.Locator() || got.LastWriter != handle {
		t.Fatalf("report artifact: %+v", got)
	}
	if _, waiting := current.Waiting.Current["child_completion:"+handle]; waiting {
		t.Fatal("child wait survived the terminal checkpoint")
	}
	mustCheckpoint(t, store.AppendTurn(ctx, id, session.Turn{ID: "resumed", PreviousTurnID: "turn"}))
	activity(llm.ActivityRecord{Kind: "usage_limit_wait"})
	if current = state(); current.Waiting.Processed.Count != 3 || len(current.Waiting.Current) != 1 {
		t.Fatalf("settled waits: %+v", current.Waiting)
	}
	want := mustContextJSON(t, current)
	for _, checkpointed := range []bool{false, true} {
		reopened, err := New(store.directory)
		mustCheckpoint(t, err)
		if checkpointed {
			view, err := reopened.OpenCheckpointedRestoreView(ctx, id)
			mustCheckpoint(t, err)
			defer view.Close()
			if view.index.savedSize == 0 {
				t.Fatal("machine replay missed the checkpoint")
			}
			_, _, err = view.Take(ctx, id)
			mustCheckpoint(t, err)
		}
		replayed, err := reopened.ContextState(ctx, id)
		mustCheckpoint(t, err)
		if got := mustContextJSON(t, replayed); !bytes.Equal(want, got) {
			t.Fatalf("checkpointed=%v\nlive: %s\nreplay: %s", checkpointed, want, got)
		}
	}
}

func TestContextMachineTerminalAndWriteFacts(t *testing.T) {
	ctx := t.Context()
	store, id, _ := newRestoreFixture(t)
	for _, status := range []operation.Status{operation.StatusFailed, operation.StatusCanceled} {
		handle := string(status)
		op, job := contextJob(t, store, id, operation.ID(handle), "SubagentStart", "subagent", childPlan{Action: "start", ParentID: string(id), Template: "fixture"})
		if status == operation.StatusCanceled {
			raw := mustContextJSON(t, inbox.ControlMessage{Mode: inbox.CancelOperation, Parameters: inbox.CancelRequest{OperationID: handle, By: "user"}})
			mustCheckpoint(t, store.AppendInput(ctx, id, inbox.Input{ID: "cancel", Kind: inbox.InputControl, Payload: raw}))
		}
		report := record.Address{Session: string(identity.ChildID(id, op.ID)), Kind: record.ChildReport, Sequence: 1, Handle: handle}.Locator()
		job.Handle = mustContextJSON(t, childHandle{Version: 1, FinishLocator: report, Finish: &sessionstore.FinishRecord{
			Version: 1, OperationID: "finish", Result: sessionstore.FinishResult{Status: "failed", Summary: "fixture"},
		}})
		step, err := operation.UpdateRemoteJob(op, job, status)
		mustCheckpoint(t, err)
		loc, err := store.SaveOperationRecorded(ctx, id, *step.Operation)
		mustCheckpoint(t, err)
		v, err := store.ContextState(ctx, id)
		mustCheckpoint(t, err)
		if status == operation.StatusCanceled {
			if _, current := v.Children.Current[handle]; current || v.Children.Processed.Count != 1 ||
				v.Children.ByEnding[0].EndedBy != contextstate.EndedByUserCancel {
				t.Fatalf("canceled child: %+v", v.Children)
			}
			if _, ok := v.Children.Processed.Sources[report]; !ok {
				t.Fatal("canceled child lost its committed Finish")
			}
		} else if got := v.Children.Current[handle]; got.Report != report || got.State != contextstate.ChildFailed ||
			got.EndedBy != contextstate.EndedByFailure || got.LastEvent != loc {
			t.Fatalf("failed child: %+v", got)
		}
	}
	op, job := contextJob(t, store, id, "write", "write", "native_file", map[string]string{"action": "write", "path": "artifact.md"})
	step, err := operation.UpdateRemoteJob(op, job, operation.StatusAwaiting)
	mustCheckpoint(t, err)
	open, err := store.SaveOperationRecorded(ctx, id, *step.Operation)
	mustCheckpoint(t, err)
	current, err := store.ContextState(ctx, id)
	mustCheckpoint(t, err)
	if got := current.Waiting.Current["operation:write"]; !slices.Contains(got.Sources, open) {
		t.Fatalf("operation wait: %+v", got)
	}
	job.Handle = []byte(`{"mutation":{"targets":[{"path":"artifact.md","code":"applied"},{"path":"no-write.md","code":"not_applied"}]}}`)
	step, err = operation.UpdateRemoteJob(op, job, operation.StatusCompleted)
	mustCheckpoint(t, err)
	write, err := store.SaveOperationRecorded(ctx, id, *step.Operation)
	mustCheckpoint(t, err)
	current, err = store.ContextState(ctx, id)
	mustCheckpoint(t, err)
	if got := current.Artifacts.Current["artifact.md"]; got.LastWriter != "self" || got.Locator != write || len(current.Artifacts.Current) != 1 {
		t.Fatalf("confirmed writes: %+v", current.Artifacts)
	}
	for _, loc := range []record.Locator{open, write} {
		if _, ok := current.Waiting.Processed.Sources[loc]; !ok {
			t.Fatal("resolved operation wait lost evidence", loc)
		}
	}
}

func contextJob(t *testing.T, store *Store, id session.ID, handle operation.ID, name, kind string, plan any) (operation.Operation, operation.RemoteJobState) {
	t.Helper()
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: operation.RemoteJobPlanType(kind), Version: 1, Data: mustContextJSON(t, plan)})
	mustCheckpoint(t, err)
	raw := mustContextJSON(t, inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(handle), ToolName: name, Spec: mustContextJSON(t, spec)}})
	in := inbox.Input{ID: inbox.ID(handle), Kind: inbox.InputControl, Payload: raw}
	mustCheckpoint(t, store.AppendInput(t.Context(), id, in))
	op, err := sessionstore.DecodeOperationIntent(in)
	mustCheckpoint(t, err)
	job, err := operation.DecodeRemoteJobState(op)
	mustCheckpoint(t, err)
	return op, job
}

func mustContextJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	mustCheckpoint(t, err)
	return raw
}
