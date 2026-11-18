package testsession

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

type WorkerTierFixture struct {
	Lead    session.ID
	Workers map[string]session.ID
	At      time.Time
}

// WriteWorkerTierTree uses synthetic, deterministic logs and no inference.
// The caller can archive any of its settled children before inspecting them.
func WriteWorkerTierTree(tb testing.TB, directory string) WorkerTierFixture {
	tb.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		tb.Fatal(err)
	}
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	f := WorkerTierFixture{Lead: subagent.ChildID("main", "lead-start"), Workers: map[string]session.ID{}, At: at}
	runtime := func(provider, model string) []byte {
		raw, _ := json.Marshal(map[string]any{"Provider": map[string]any{"provider": provider, "model": map[string]string{"id": model}}, "ReasoningEffort": "max"})
		return raw
	}
	itemAt := func(log *settledLog, second int, kind sessionstore.ItemKind, data any) {
		log.at = at.Add(time.Duration(second)*time.Second - time.Second)
		log.item(kind, data)
	}
	record := func(log *settledLog, second int, r sessionstore.HostRecord) {
		itemAt(log, second, sessionstore.ItemHostRecord, r)
	}
	activity := func(log *settledLog, second int, a llm.ActivityRecord) {
		record(log, second, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity, Activity: &a})
	}
	response := func(log *settledLog, second int, model string, input, output, cached int64, duration int64, source string) {
		itemAt(log, second-1, sessionstore.ItemTurn, session.Turn{ID: "turn", Type: session.TurnRegular})
		r := llm.Response{Model: model, Usage: llm.Usage{InputTokens: input, OutputTokens: output, CachedInputTokens: cached, Raw: []byte(`{}`)}}
		if duration > 0 {
			r.Generation = &llm.GenerationStatistics{Model: model, Tokens: llm.ReportedTokens(r.Usage), DurationMicros: duration * 1e6, Source: source}
		}
		itemAt(log, second, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "turn", Response: r})
	}
	finish := func(log *settledLog, second int, status string) *sessionstore.FinishRecord {
		f := &sessionstore.FinishRecord{Version: 1, ModelTurnID: "turn", Result: sessionstore.FinishResult{Status: status, Summary: "synthetic result", Tests: []string{"fixture test: PASS"}}}
		record(log, second, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: f})
		return f
	}
	parent := &settledLog{tb: tb, at: at.Add(-time.Second)}
	parent.header("main", jsontext.Value(runtime("claude-code", "opus-fixture")))
	lead := &settledLog{tb: tb, at: at}
	leadRuntime := runtime("openai-codex", "gpt-6.2-sol")
	lead.header(f.Lead, subagent.ChildConfig{Version: 1, ParentID: "main", ChildID: f.Lead, OperationID: "lead-start", Template: "lead", SessionDirectory: directory, Workspace: directory, Runtime: leadRuntime, ReadyID: "ready:lead-start", Task: "Issue owner", Policy: permission.Config{Tools: []string{"Finish"}}})
	response(parent, 2, "opus-fixture", 500, 100, 0, 1, "provider_api")
	response(lead, 3, "gpt-6.2-sol", 500, 100, 0, 2, "provider_api")
	start := func(log *settledLog, parentID session.ID, handle operation.ID, template, title string, raw []byte, second int, status operation.Status, finished *sessionstore.FinishRecord, legacy bool) {
		selection := sessionstore.SelectionFromConfiguration(raw)
		a := llm.ActivityRecord{Kind: "child_start", ID: string(handle), Template: template, Provider: selection.Provider, Model: selection.Model, Effort: string(selection.Effort), StartedAt: at.Add(time.Duration(second) * time.Second), Layer: subagent.RoleGroup(template)}
		if legacy {
			a.Layer = ""
		}
		activity(log, second, a)
		spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: parentID, Template: template, Text: title + "\nprivate-task-body-sensitive", Configuration: &subagent.Template{Workspace: directory, Runtime: raw, Policy: permission.Config{Tools: []string{"Finish"}}}, Notify: true})
		if err != nil {
			tb.Fatal(err)
		}
		encoded, _ := json.Marshal(spec)
		payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(handle), ToolName: "SubagentStart", Spec: encoded}})
		itemAt(log, second, sessionstore.ItemInput, inbox.Input{ID: inbox.ID(handle), Kind: inbox.InputControl, Payload: payload})
		state, err := operation.DecodeRemoteJobState(operation.Operation{Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength})
		if err != nil {
			tb.Fatal(err)
		}
		id := subagent.ChildID(parentID, handle)
		state.Handle, _ = json.Marshal(subagent.Handle{Version: 1, ParentID: parentID, ChildID: id, OperationID: handle, Finish: finished})
		data, _ := json.Marshal(state)
		log.line("operation", struct{ Operation operation.Operation }{operation.Operation{ID: handle, ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: data, Status: status, MaxOutputLength: spec.MaxOutputLength}})
		peer, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(id), Target: string(parentID), Handle: string(handle), Kind: "ended", Text: "synthetic child ended"})
		itemAt(log, second, sessionstore.ItemInput, inbox.Input{ID: inbox.ID("ended:" + string(handle)), Kind: inbox.InputPeer, Payload: peer})
	}
	for _, c := range []struct {
		handle, model, title, status, review, source string
		start, response, end                         int
		input, output, cached, duration              int64
		legacy, wait                                 bool
	}{
		{"flash-first", "deepseek-v4.1-flash", "Parser task", "completed", "reject", "provider_api", 10, 12, 30, 1000, 600, 200, 2, false, false},
		{"qwen-archived", "qwen3.8-flash", "Query task", "completed", "accept", "client_clock", 11, 15, 31, 2000, 500, 300, 5, false, false},
		{"flash-rework", "deepseek-v4.1-flash", "Parser task", "completed", "accept", "provider_api", 40, 44, 60, 1000, 600, 200, 2, false, true},
		{"qwen-no-timing", "qwen3.8-flash", "Missing timing", "completed", "", "", 70, 80, 90, 500, 300, 0, 0, true, false},
		{"flash-failed", "deepseek-v4.1-flash", "Failed task", "failed", "reject", "provider_api", 100, 105, 110, 100, 100, 0, 1, false, false},
		{"qwen-canceled", "qwen3.8-flash", "Canceled task", "canceled", "accept", "", 120, 0, 130, 0, 0, 0, 0, false, false},
	} {
		handle := operation.ID(c.handle)
		id := subagent.ChildID(f.Lead, handle)
		f.Workers[c.handle] = id
		raw := runtime("opencode-go", c.model)
		child := &settledLog{tb: tb, at: at.Add(time.Duration(c.start) * time.Second)}
		child.header(id, subagent.ChildConfig{Version: 1, ParentID: f.Lead, ChildID: id, OperationID: handle, Template: "worker-max", SessionDirectory: directory, Workspace: directory, Runtime: raw, ReadyID: inbox.ID("ready:" + c.handle), Task: c.title, Policy: permission.Config{Tools: []string{"Finish"}}})
		if c.wait {
			activity(child, 41, llm.ActivityRecord{Kind: "usage_limit_wait", StartedAt: at.Add(41 * time.Second), RetryAt: at.Add(43 * time.Second)})
			activity(child, 43, llm.ActivityRecord{Kind: "usage_limit_wait", StartedAt: at.Add(43 * time.Second)})
		}
		var done *sessionstore.FinishRecord
		if c.response > 0 {
			response(child, c.response, c.model, c.input, c.output, c.cached, c.duration, c.source)
			done = finish(child, c.end, c.status)
		} else {
			record(child, c.end, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostEnded, End: &sessionstore.EndRecord{EndedBy: "host_restart", EndedAt: at.Add(time.Duration(c.end) * time.Second)}})
		}
		child.write(directory, id)
		start(lead, f.Lead, handle, "worker-max", c.title, raw, c.start, operation.Status(c.status), done, c.legacy)
		if c.review != "" {
			record(lead, c.end+1, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostChildReview, ChildReview: &sessionstore.ChildReview{InputID: inbox.ID("review:" + c.handle), Handle: c.handle, ChildID: string(id), Decision: c.review, Reason: "review-reason-sensitive"}})
		}
	}
	leadFinish := finish(lead, 140, "completed")
	lead.write(directory, f.Lead)
	start(parent, "main", "lead-start", "lead", "Issue owner", leadRuntime, 0, operation.StatusCompleted, leadFinish, false)
	itemAt(parent, 150, sessionstore.ItemTurn, session.Turn{ID: "root-tail", PreviousTurnID: "turn", Type: session.TurnRegular})
	itemAt(parent, 151, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "root-tail", Response: llm.Response{Model: "opus-fixture", Usage: llm.Usage{InputTokens: 50, OutputTokens: 10}}})
	parent.write(directory, "main")
	return f
}
