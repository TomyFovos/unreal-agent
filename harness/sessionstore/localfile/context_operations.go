package localfile

import (
	"encoding/json/v2"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (head *sessionHead) applyOperationRecord(r operationRecord, offset int64) error {
	op := r.Operation
	o := contextObserver{head: head, at: r.RecordedAt}
	o.source = record.Address{Kind: record.OperationCheckpoint, Session: o.id(), Operation: string(op.ID), Offset: offset}.Locator()
	previous := o.lookupOperation(op.ID)
	if err := head.saveOperation(op); err != nil {
		return err
	}
	o.operation(op, previous)
	return o.err
}

func (o *contextObserver) operation(op, previous operation.Operation) {
	var job operation.RemoteJobState
	if op.Type == operation.TypeRemoteJob {
		job, _ = operation.DecodeRemoteJobState(op)
		if plan, ok := decodeChildPlan(job); ok {
			o.childOperation(op, previous, job, plan)
			if plan.Action == "start" {
				return // The child-completion wait uses the handle.
			}
		}
	}
	switch {
	case op.Status == operation.StatusAwaiting && previous.Status != operation.StatusAwaiting:
		o.wait(contextstate.WaitOperation, string(op.ID), "", op.ToolName, job.NextInspectionAt, false)
	case terminalOperationStatus(op.Status) && !terminalOperationStatus(previous.Status):
		o.wait(contextstate.WaitOperation, string(op.ID), "", "", time.Time{}, true)
		if o.head.ContextFacts.Parent != "" {
			if rule := sessionstore.AutomaticPermissionRule(op); rule != "" {
				o.recent("permission_routed", op.ToolName, o.head.ContextFacts.ParentHandle, rule)
			}
		}
		if op.Status == operation.StatusCompleted {
			o.operationArtifacts(op, job)
		}
	}
}

func (o *contextObserver) operationArtifacts(op operation.Operation, job operation.RemoteJobState) {
	if op.Type == operation.TypeValue {
		for _, path := range operation.RecordedWritePaths(op) {
			o.artifact(path, "self", o.source)
		}
		return
	}
	if job.Plan.Type != "native_file" && job.Plan.Type != "structural_code" {
		return
	}
	var result struct {
		Mutation *mutation.Result `json:"mutation"`
	}
	if json.Unmarshal(job.Handle, &result) != nil || result.Mutation == nil {
		return
	}
	for _, target := range result.Mutation.Targets {
		if target.Code == mutation.Applied {
			o.artifact(target.Path, "self", o.source)
		}
	}
}
