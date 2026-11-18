package localfile

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
)

// Read the observed wire fields without depending on the child executor,
// which itself uses the Session store.
type childBrief struct {
	Title      string `json:"title"`
	Purpose    string `json:"purpose"`
	Outputs    string `json:"outputs"`
	Verify     string `json:"verification"`
	Acceptance string `json:"acceptance_criteria"`
}

type childPlan struct {
	Action, ParentID, Template, Handle, Text, Request string
	Allow                                             bool
	Configuration                                     *struct{ Runtime jsontext.Value }
	Brief                                             childBrief
}

type childHandle struct {
	Version       uint32
	Brief         *struct{ Fields childBrief }
	Finish        *sessionstore.FinishRecord
	FinishLocator record.Locator
	Approvals     []childApproval
}

type childApproval struct{ ID, Tool, Scope string }

type childCancellation struct {
	EndedBy contextstate.Ending
	Source  record.Locator
}

func decodeChildPlan(state operation.RemoteJobState) (childPlan, bool) {
	var p childPlan
	ok := state.Plan.Type == "subagent" && state.Plan.Version == 1 && json.Unmarshal(state.Plan.Data, &p) == nil
	return p, ok
}

func (o *contextObserver) lookupOperation(id operation.ID) operation.Operation {
	if index, ok := o.head.operationPositions[id]; ok {
		return o.head.Operations[index]
	}
	return operation.Operation{}
}

func (o *contextObserver) hasLiveChildOperation() bool {
	return slices.ContainsFunc(o.head.Operations, func(op operation.Operation) bool {
		return op.ToolName == "SubagentStart" && op.Type == operation.TypeRemoteJob && !terminalOperationStatus(op.Status)
	})
}

func (o *contextObserver) childStarted(a *llm.ActivityRecord) {
	op := o.lookupOperation(operation.ID(a.ID))
	if op.ID == "" || terminalOperationStatus(op.Status) {
		return
	}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return
	}
	p, ok := decodeChildPlan(state)
	if !ok || p.Action != "start" || p.ParentID != o.id() {
		return
	}
	child := o.child(a.ID, p)
	if child.Started.IsZero() {
		child.Started = a.StartedAt
	}
	if a.Model != "" {
		child.Model, child.Effort = a.Model, a.Effort
	}
	o.saveChild(child, childCancellation{})
}

func (o *contextObserver) child(handle string, p childPlan) contextstate.Child {
	if child, ok := o.state().Children.Current[handle]; ok {
		return child
	}
	child := contextstate.Child{Handle: handle, Template: p.Template, State: contextstate.ChildRunning}
	if p.Configuration != nil {
		if selection := sessionstore.SelectionFromConfiguration(p.Configuration.Runtime); selection != nil {
			child.Model, child.Effort = selection.Model, string(selection.Effort)
		}
	}
	setChildBrief(&child, p.Brief, p.Text)
	return child
}

func setChildBrief(child *contextstate.Child, brief childBrief, task string) {
	child.Title = firstContextLine(brief.Title)
	if child.Title == "" {
		child.Title = firstContextLine(task)
	}
	child.Purpose, child.Outputs = brief.Purpose, brief.Outputs
	child.Verification, child.AcceptanceCriteria = brief.Verify, brief.Acceptance
}

func (o *contextObserver) saveChild(child contextstate.Child, cancel childCancellation) {
	child.LastEvent = o.source
	o.add(contextstate.ChildChanged, child.Handle, contextstate.ChildUpdate{Child: child, CancelRecorded: cancel.Source != ""}, cancel.Source)
}

func (o *contextObserver) reviewChild(review sessionstore.ChildReview) {
	op := o.lookupOperation(operation.ID(review.Handle))
	job, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return
	}
	p, ok := decodeChildPlan(job)
	if !ok || p.Action != "start" || p.ParentID != o.id() {
		return
	}
	child := o.child(review.Handle, p)
	child.Accept = contextstate.AcceptAccepted
	if review.Decision == "reject" {
		child.Accept = contextstate.AcceptRejected
	}
	o.saveChild(child, childCancellation{})
}

func liveChild(child contextstate.Child) bool {
	return child.State == contextstate.ChildRunning || child.State == contextstate.ChildAsked || child.State == contextstate.ChildWaitingApproval
}

func (o *contextObserver) childInput(id inbox.ID, p inbox.PeerMessage) {
	if p.Sender != string(identity.ChildID(session.ID(p.Target), operation.ID(p.Handle))) {
		return // The child's own tasks and parent replies are other Peer inputs.
	}
	switch p.Kind {
	case "progress", "ready":
		o.recent("child_"+p.Kind, p.Text, p.Handle, "P1")
	case "message":
		if strings.HasPrefix(string(id), "approval:") {
			o.recent("approval_forwarded", "", p.Handle, "A3")
			return
		}
		o.recent("child_question", p.Text, p.Handle, p.RuleID)
		if child := o.state().Children.Current[p.Handle]; liveChild(child) {
			child.UnansweredQuestions = append(slices.Clone(child.UnansweredQuestions), o.source)
			if child.State != contextstate.ChildWaitingApproval {
				child.State = contextstate.ChildAsked
			}
			o.saveChild(child, childCancellation{})
		}
	}
}

func (o *contextObserver) operationIntent(op operation.Operation) {
	if op.ToolName != "SubagentSend" {
		return
	}
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return
	}
	p, ok := decodeChildPlan(state)
	if !ok || p.Action != "send" || p.ParentID != o.id() {
		return
	}
	if o.head.ContextFacts.Replies == nil {
		o.head.ContextFacts.Replies = make(map[operation.ID][]record.Locator)
	}
	o.head.ContextFacts.Replies[op.ID] = slices.Clone(o.state().Children.Current[p.Handle].UnansweredQuestions)
}

func (o *contextObserver) cancelChild(id operation.ID, by string) {
	op := o.lookupOperation(id)
	if op.ToolName != "SubagentStart" || terminalOperationStatus(op.Status) {
		return
	}
	ending := contextstate.Ending("")
	switch by {
	case "coordinator":
		ending = contextstate.EndedByCoordinatorCancel
	case "user":
		ending = contextstate.EndedByUserCancel
	}
	if o.head.ContextFacts.Cancels == nil {
		o.head.ContextFacts.Cancels = make(map[operation.ID]childCancellation)
	}
	if _, exists := o.head.ContextFacts.Cancels[id]; !exists {
		o.head.ContextFacts.Cancels[id] = childCancellation{EndedBy: ending, Source: o.source}
	}
}

func (o *contextObserver) childOperation(op, previous operation.Operation, state operation.RemoteJobState, p childPlan) {
	if p.ParentID != o.id() && p.Action != "parent" {
		return
	}
	if p.Action == "start" {
		o.childCheckpoint(op, previous, state, p)
		return
	}
	if !terminalOperationStatus(op.Status) || terminalOperationStatus(previous.Status) {
		return
	}
	switch p.Action {
	case "send":
		questions, started := o.head.ContextFacts.Replies[op.ID]
		delete(o.head.ContextFacts.Replies, op.ID)
		if child := o.state().Children.Current[p.Handle]; started && liveChild(child) &&
			op.Status == operation.StatusCompleted && state.TerminalResult == "Subagent send acknowledged" {
			child.Sends++
			child.UnansweredQuestions = slices.DeleteFunc(slices.Clone(child.UnansweredQuestions), func(q record.Locator) bool {
				return slices.Contains(questions, q)
			})
			if child.State == contextstate.ChildAsked && len(child.UnansweredQuestions) == 0 {
				child.State = contextstate.ChildRunning
			}
			o.saveChild(child, childCancellation{})
		}
	case "approve":
		if op.Status == operation.StatusCompleted {
			decision := "denied"
			if p.Allow {
				decision = "allow_once"
			}
			id := childApprovalID(p.Handle, p.Request)
			// The child's pending list can close before the explicit answer's
			// Operation completes. Both records are evidence of the same wait.
			_, open := o.state().Approvals.Current[id]
			o.add(contextstate.ApprovalChanged, id, contextstate.ApprovalUpdate{
				Approval:      contextstate.Approval{ID: id, Handle: p.Handle, Decision: decision, TS: o.at, Locator: o.source},
				AlreadyClosed: !open,
			})
			o.recent("approval_answered", decision, p.Handle, "")
		}
	case "parent":
		var receipt struct {
			RuleID string `json:"rule_id"`
			Answer string `json:"answer"`
		}
		if op.Status == operation.StatusCompleted && json.Unmarshal(state.Handle, &receipt) == nil && receipt.RuleID != "" && receipt.Answer != "" {
			o.recent("routing_answered", "", o.head.ContextFacts.ParentHandle, receipt.RuleID)
		}
	}
}

func childApprovalID(handle, id string) string {
	return handle + ":" + id
}

func (o *contextObserver) childCheckpoint(op, previous operation.Operation, job operation.RemoteJobState, p childPlan) {
	if terminalOperationStatus(previous.Status) {
		return
	}
	var wire childHandle
	if len(job.Handle) != 0 {
		_ = json.Unmarshal(job.Handle, &wire)
	}
	ended := terminalOperationStatus(op.Status)
	if !ended && (op.Status != operation.StatusAwaiting || wire.Version != 1) {
		return
	}
	handle := string(op.ID)
	child := o.child(handle, p)
	if wire.Brief != nil {
		setChildBrief(&child, wire.Brief.Fields, p.Text)
	}
	o.childApprovals(handle, wire.Approvals, ended)
	if !ended {
		if child.Started.IsZero() {
			child.Started = o.at
		}
		child.State = contextstate.ChildRunning
		if len(child.UnansweredQuestions) > 0 {
			child.State = contextstate.ChildAsked
		}
		if len(wire.Approvals) > 0 {
			child.State = contextstate.ChildWaitingApproval
		}
		o.saveChild(child, childCancellation{})
		return
	}

	cancel := o.head.ContextFacts.Cancels[op.ID]
	delete(o.head.ContextFacts.Cancels, op.ID)
	child.Ended, child.UnansweredQuestions = o.at, nil
	child.Report = wire.FinishLocator
	switch op.Status {
	case operation.StatusCompleted:
		child.State, child.EndedBy = contextstate.ChildFinished, contextstate.EndedByFinish
	case operation.StatusFailed:
		child.State, child.EndedBy = contextstate.ChildFailed, contextstate.EndedByFailure
		child.ResultFirstLine = firstContextLine(job.TerminalError)
	case operation.StatusCanceled:
		child.State, child.EndedBy = contextstate.ChildCancelled, cancel.EndedBy
	}
	if op.Status != operation.StatusCanceled {
		cancel = childCancellation{}
	}
	if wire.Finish != nil {
		child.ResultFirstLine = firstContextLine(wire.Finish.Result.Summary)
		if child.Report == "" {
			child.Report = o.source // Older checkpoints retain the full report here.
		}
	}
	o.saveChild(child, cancel)
	if wire.Finish == nil {
		return
	}
	o.recent("finish_delivered", "", handle, "F1", child.Report)
	loc := o.source
	if addr, err := child.Report.Address(); err == nil && addr.Kind == record.ChildReport {
		addr.Section = "changed-files"
		loc = addr.Locator()
	}
	for _, path := range wire.Finish.Result.ChangedFiles {
		o.artifact(path, handle, loc)
	}
}

func (o *contextObserver) childApprovals(handle string, pending []childApproval, ended bool) {
	for id, old := range o.state().Approvals.Current {
		if old.Handle == handle && (ended || !slices.ContainsFunc(pending, func(a childApproval) bool {
			return childApprovalID(handle, a.ID) == id
		})) {
			o.approval(id, handle, "", "", "closed")
		}
	}
	if ended {
		return
	}
	for _, a := range pending {
		id := childApprovalID(handle, a.ID)
		if _, open := o.state().Approvals.Current[id]; !open {
			o.approval(id, handle, a.Tool, a.Scope, "")
		}
	}
}
