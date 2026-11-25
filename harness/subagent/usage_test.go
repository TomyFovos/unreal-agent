package subagent_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestUsageTreeIncludesArchivedChildAndGrandchildAfterReplay(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := localfile.New(dir)
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	require(err)
	cfg := jsontext.Value(`{"Provider":{"provider":"opencode-go","model":{"id":"green"}},"ReasoningEffort":"high"}`)
	create := func(id session.ID, configuration jsontext.Value) {
		_, err := store.Create(ctx, id)
		require(err)
		require(store.AppendHostRecord(ctx, id, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: configuration}))
	}
	create("root", cfg)
	attach := func(parent session.ID, handle operation.ID, role string, budget int) session.ID {
		child := sub.ChildID(parent, handle)
		spec, err := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: parent, Template: role,
			Configuration: &sub.Template{Workspace: dir, Runtime: cfg, Layer: role}, Brief: sub.BriefFields{BudgetSteps: budget}})
		require(err)
		raw, err := json.Marshal(spec)
		require(err)
		intent, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{
			OperationID: string(handle), ToolName: "SubagentStart", Spec: raw}})
		require(err)
		require(store.AppendInput(ctx, parent, inbox.Input{Kind: inbox.InputControl, ID: inbox.ID("start:" + string(handle)), Payload: intent}))
		op := operation.Operation{ID: handle, Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, ToolName: "SubagentStart", Status: operation.StatusReady}
		require(store.SaveOperation(ctx, parent, op))
		configuration, err := json.Marshal(struct {
			ParentID session.ID
			Layer    string
			Runtime  jsontext.Value
		}{parent, role, cfg})
		require(err)
		create(child, configuration)
		op.Status = operation.StatusCompleted
		require(store.SaveOperation(ctx, parent, op))
		return child
	}
	lead := attach("root", "lead", "lead", 5)
	worker := attach(lead, "worker", "worker", 3)
	previous := make(map[session.ID]session.TurnID)
	add := func(id session.ID, turn session.TurnID, provider string, u llm.Usage) {
		require(store.AppendTurn(ctx, id, session.Turn{ID: turn, PreviousTurnID: previous[id], Type: session.TurnRegular}))
		previous[id] = turn
		require(store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{
			ID: string(turn), Provider: provider, Model: "green", Stop: llm.StopComplete, Usage: u}}))
	}
	u := llm.Usage{InputTokens: 100, CachedInputTokens: 80, CacheWriteInputTokens: 10, OutputTokens: 12, ReasoningTokens: 2}
	add("root", "main", "fake", u)
	u.Raw = jsontext.Value(`{"opencode":{"billing_source":"go","allowance":0.6,"list_cost":1}}`)
	add(lead, "before", "opencode-go", u)
	u.Charge = &llm.Charge{Currency: "USD", BillingSource: "go-plus", Charged: new(1.2), List: new(1.0)}
	add(lead, "after", "opencode-go", u)
	u.Charge, u.Raw = nil, nil
	add(worker, "work", "opencode-go", u)
	for _, archive := range []localfile.ArchiveEntry{
		{ChildID: worker, ParentID: lead, OperationID: "worker"},
		{ChildID: lead, ParentID: "root", OperationID: "lead"},
	} {
		archive.EndedAt, archive.Status = time.Now(), operation.StatusCompleted
		ok, err := store.ArchiveChild(ctx, archive)
		if err != nil || !ok {
			t.Fatalf("archive %s: %t %v", archive.ChildID, ok, err)
		}
	}
	now := time.Now()
	var first sub.UsageReport
	for range 2 {
		reader, err := sub.OpenUsageReader(dir)
		require(err)
		report, err := reader.Read(ctx, "root", now, time.Hour)
		require(err)
		if report.Partial || report.All.Totals.Calls != 4 || report.Last10m.Totals.Calls != 4 ||
			report.Own.Last10m.Totals.Calls != 1 || report.Own.Last60m.ByProvider[0].Provider != "fake" ||
			report.All.Totals.Uncached.Value == nil || *report.All.Totals.Uncached.Value != 40 ||
			report.Sessions[0].Own.Calls != 1 || *report.Sessions[0].Own.Input.Value != 100 ||
			len(report.TopChildren) != 1 || report.TopChildren[0].Subtree.Calls != 3 || report.TopChildren[0].BudgetSteps != 5 {
			t.Fatalf("incorrect tree: %+v", report)
		}
		goUsage := report.All.ByProvider[1]
		if goUsage.Provider != "opencode-go" || goUsage.ChargeUnknown != 1 || len(goUsage.Charges) != 3 ||
			goUsage.Charges[1].BillingSource != "go" || *goUsage.Charges[1].Charged.Value != 0.6 ||
			goUsage.Charges[2].BillingSource != "go-plus" || *goUsage.Charges[2].Charged.Value != 1.2 ||
			report.All.ByProvider[0].DollarText() != "unknown" {
			t.Fatalf("incorrect provider charges: %+v", report.All.ByProvider)
		}
		if first.Root != "" && !reflect.DeepEqual(report, first) {
			t.Fatal("read after restore changed the ledger")
		}
		first = report
	}
}
