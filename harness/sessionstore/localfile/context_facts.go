package localfile

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Context events derived from canonical facts commit with their source. Both
// the live write head and replay run these transitions, so a crash cannot
// leave a committed source without its L2 update.
type contextFacts struct {
	Parent       session.ID
	ParentHandle string
	Replies      map[operation.ID][]record.Locator  `json:",omitempty"`
	Cancels      map[operation.ID]childCancellation `json:",omitempty"`
}

type contextObserver struct {
	head   *sessionHead
	at     time.Time
	source record.Locator
	err    error
}

func (o *contextObserver) add(kind contextstate.EventKind, key string, value any, sources ...record.Locator) {
	if o.err != nil {
		return
	}
	id := fmt.Sprintf("fact:%s:%s:%s", o.source, kind, key)
	ev, err := contextstate.NewEvent(id, kind, value, sources...)
	if err == nil {
		ev.Record = o.source
		err = o.head.ContextProjection.Apply(ev)
	}
	o.err = err
}

func (o *contextObserver) state() *contextstate.State { return &o.head.ContextProjection.State }
func (o *contextObserver) id() string                 { return string(o.head.Snapshot.Session.ID) }

func (o *contextObserver) recent(kind, text, handle, rule string, sources ...record.Locator) {
	o.add(contextstate.RecentObserved, kind+":"+handle+":"+rule, contextstate.RecentEvent{
		TS: o.at, Kind: kind, FirstLine: firstContextLine(text), Locator: o.source, Handle: handle, RuleID: rule,
	}, sources...)
}

func (o *contextObserver) wait(kind contextstate.WaitKind, id, handle, text string, retryAt time.Time, resolved bool) {
	if resolved {
		if _, open := o.state().Waiting.Current[string(kind)+":"+id]; !open {
			return
		}
	}
	o.add(contextstate.WaitChanged, string(kind)+":"+id, contextstate.WaitUpdate{
		Wait: contextstate.Wait{ID: id, Kind: kind, TS: o.at, Handle: handle, Text: text, RetryAt: retryAt}, Resolved: resolved,
	})
}

func (o *contextObserver) artifact(path, writer string, source record.Locator) {
	if path != "" {
		o.add(contextstate.ArtifactChanged, path, contextstate.Artifact{Path: path, LastWriter: writer, TS: o.at, Locator: source}, source)
	}
}

func firstContextLine(text string) string {
	line, _, newline := strings.Cut(text, "\n")
	if newline {
		return strings.TrimSuffix(line, "\r")
	}
	return line
}

func (head *sessionHead) observeItem(r itemRecord) error {
	item := r.Item
	o := contextObserver{head: head, at: item.RecordedAt, source: record.Item(string(head.Snapshot.Session.ID), uint64(item.Sequence))}
	switch v := item.Data.(type) {
	case inbox.Input:
		o.input(item.Sequence, v)
	case session.Turn:
		o.wait(contextstate.WaitParentFailure, "parent", "", "", time.Time{}, true)
	case sessionstore.ModelResponse:
		o.questions(item.Sequence, v)
		if failure := v.Response.Failure; failure != nil && o.hasLiveChildOperation() {
			o.wait(contextstate.WaitParentFailure, "parent", "", failure.Code, time.Time{}, false)
		}
	case sessionstore.HostRecord:
		o.hostRecord(item.Sequence, v)
	case sessionstore.ToolCallStatus:
		for _, op := range r.Operations {
			o.operationIntent(op)
		}
	}
	return o.err
}

func (o *contextObserver) input(seq sessionstore.Sequence, v inbox.Input) {
	switch v.Kind {
	case inbox.InputExternal:
		var text string
		if json.Unmarshal(v.Payload, &text) != nil {
			return
		}
		o.source = record.Input(o.id(), uint64(seq), record.UserMessage)
		o.add(contextstate.RequestChanged, string(v.ID), contextstate.Request{
			ID: string(v.ID), TS: o.at, Origin: contextstate.HumanInput, Locator: o.source, Text: text,
		})
		o.recent("user_message", "", "", "U1")
	case inbox.InputPeer:
		p, err := v.DecodePeerMessage()
		if err == nil && p.Target == o.id() {
			o.source = record.Input(o.id(), uint64(seq), record.PeerMessage)
			o.childInput(v.ID, p)
		}
	case inbox.InputControl:
		msg, err := v.DecodeControlMessage()
		if err != nil {
			return
		}
		switch msg.Mode {
		case inbox.DispatchOperation:
			if op, err := sessionstore.DecodeOperationIntent(v); err == nil {
				o.operationIntent(op)
			}
		case inbox.CancelOperation:
			cancel := msg.Parameters.(inbox.CancelRequest)
			o.cancelChild(operation.ID(cancel.OperationID), cancel.By)
		case inbox.CancelChildren, inbox.StopHard:
			by := "user"
			if o.head.ContextFacts.Parent != "" {
				by = "coordinator"
			}
			for _, op := range o.head.Operations {
				o.cancelChild(op.ID, by)
			}
		}
	}
}

func (o *contextObserver) questions(seq sessionstore.Sequence, v sessionstore.ModelResponse) {
	for part, item := range v.Response.Output {
		m, ok := item.Data.(llm.Message)
		if !ok || m.Role != llm.RoleAssistant {
			continue
		}
		var lineNumber int64
		for line := range strings.SplitSeq(m.Text, "\n") {
			lineNumber++
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, contextstate.QuestionMarker) {
				continue
			}
			loc := record.Address{Session: o.id(), Sequence: uint64(seq), Kind: record.AssistantMessage, Part: part, Line: lineNumber}.Locator()
			o.add(contextstate.QuestionChanged, string(loc), contextstate.Question{
				ID: string(loc), TS: o.at, FirstLine: line, Status: contextstate.QuestionOpen, Locator: loc,
			})
		}
	}
}

func (o *contextObserver) hostRecord(seq sessionstore.Sequence, r sessionstore.HostRecord) {
	switch r.Kind {
	case "configuration":
		var lineage struct {
			ParentID, ChildID session.ID
			OperationID       operation.ID
		}
		if json.Unmarshal(r.Configuration, &lineage) == nil && string(lineage.ChildID) == o.id() {
			o.head.ContextFacts.Parent = lineage.ParentID
			o.head.ContextFacts.ParentHandle = string(lineage.OperationID)
		}
	case sessionstore.HostActivity:
		if a := r.Activity; a != nil {
			switch a.Kind {
			case "child_start":
				o.childStarted(a)
			case "usage_limit_wait":
				o.wait(contextstate.WaitUsageLimit, "model", "", "", a.RetryAt, a.RetryAt.IsZero())
			case llm.ActivityModelFailure:
				o.wait(contextstate.WaitParentFailure, "parent", "", a.Reason, time.Time{}, false)
			}
		}
	case sessionstore.HostApproval:
		if a := r.Approval; a != nil {
			o.approval(a.Request.ID, "", a.Request.Tool, a.Request.Scope, a.Decision)
		}
	case sessionstore.HostChildReview:
		o.reviewChild(*r.ChildReview)
	case sessionstore.HostArtifact:
		if r.Artifact != nil {
			o.artifact(r.Artifact.Path, "self", r.Artifact.Locator(o.head.Snapshot.Session.ID, seq))
		}
	case "finish":
		loc := o.source
		if handle := o.head.ContextFacts.ParentHandle; handle != "" {
			loc = record.Address{Session: o.id(), Sequence: uint64(seq), Kind: record.ChildReport, Handle: handle, Section: "changed-files"}.Locator()
		}
		for _, path := range r.Finish.Result.ChangedFiles {
			o.artifact(path, "self", loc)
		}
	case sessionstore.HostContext:
		if r.Context != nil && r.Context.Kind == "summary" {
			o.add(contextstate.BoundaryObserved, "", contextstate.Boundary{TS: o.at, Locator: o.source})
		}
	}
}

func (o *contextObserver) approval(id, handle, tool, scope, decision string) {
	request := tool
	if scope != "" {
		request += " " + scope
	}
	o.add(contextstate.ApprovalChanged, id, contextstate.Approval{ID: id, Handle: handle, Request: request, Decision: decision, TS: o.at, Locator: o.source})
}
