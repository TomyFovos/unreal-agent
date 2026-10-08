package analysis

import (
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"strings"
	"testing"
	"time"
)

func analysisFixture(t *testing.T) (*Accumulator, time.Time) {
	t.Helper()
	a := New("test")
	now := time.Unix(1000, 0)
	a.Created(now)
	feed := func(kind sessionstore.ItemKind, data any) {
		a.Apply(host.HistoryItem{Kind: kind, RecordedAt: now, Data: data})
		now = now.Add(time.Second)
	}
	feed(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"openai-codex","model":{"id":"model-a"},"auth":{"token":"authorization-sensitive"}},"ReasoningEffort":"medium","SystemPrompt":"prompt-sensitive"}`)})
	feed(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"input-sensitive"`)})
	feed(sessionstore.ItemTurn, session.Turn{ID: "t1", Type: session.TurnRegular})
	feed(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "t1", Response: llm.Response{Usage: llm.Usage{InputTokens: 100, CachedInputTokens: 50, CacheWriteInputTokens: 10, ReasoningTokens: 20, OutputTokens: 30}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "agent-sensitive"}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-1", Name: "read", Arguments: "tool-args-sensitive"}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-2", Name: "read"}}, {Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "call-3", Name: "grep"}}}}})
	feed(sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: "t1", CallID: "call-1"})
	feed(sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: "t1", CallID: "call-2", Status: tool.CallStatus{Error: "error-credential-sensitive"}})
	feed(sessionstore.ItemToolCallStatus, sessionstore.ToolCallStatus{TurnID: "t1", CallID: "call-3", Status: tool.CallStatus{WaitingFor: []operation.ID{"op-3"}}, Operations: []operation.Operation{{ID: "op-3", Status: operation.StatusCanceled, State: []byte(`{"body":"operation-sensitive"}`)}}})
	choice := sessionstore.RuntimeSelection{Version: 1, Revision: 1, Model: "model-b", Name: "B", Effort: llm.ReasoningEffortHigh, Provider: "openai-codex", ContextWindow: 1000}
	feed(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeSelection, Selection: &choice})
	feed(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: &choice})
	feed(sessionstore.ItemTurn, session.Turn{ID: "t2", Type: session.TurnRegular, RuntimeRevision: 1})
	feed(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "t2", Response: llm.Response{Failure: &llm.Failure{Code: "provider_failure", Message: "provider-credential-sensitive"}}})
	return a, now
}

func TestClaudeManagedPolicyOverviewAndMetadataOnlyExports(t *testing.T) {
	for _, mode := range []string{"", "reject", "trust"} {
		t.Run(mode, func(t *testing.T) {
			a := New("claude")
			configuration, err := json.Marshal(map[string]any{
				"Provider":        map[string]any{"provider": "claude-code", "model": map[string]string{"id": "configured-a"}, "auth": map[string]string{"token": "token-sensitive", "accountId": "account-sensitive"}},
				"ReasoningEffort": "medium", "SystemPrompt": "prompt-sensitive",
				"ClaudeCode": map[string]any{"managedPolicyMode": mode, "managedSettings": map[string]any{"hooks": "hook-sensitive", "env": map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "endpoint-sensitive", "OTEL_EXPORTER_OTLP_HEADERS": "credential-sensitive"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Kind: "configuration", Configuration: configuration}})
			r := a.Snapshot(time.Now(), true, false, nil, nil)
			want := mode
			if want == "" {
				want = "reject"
			}
			if r.ManagedPolicyMode != want || r.Selection == nil || r.Selection.Provider != "claude-code" {
				t.Fatal("lost runtime metadata", r)
			}
			lines := strings.Join(Lines(r, "Overview", true), "\n")
			for _, expected := range []string{"Provider    claude-code", "Managed policy mode  " + want, "Tool ownership       Unreal Agent", "Claude tools         blocked"} {
				if !strings.Contains(lines, expected) {
					t.Fatal("overview omitted safe metadata", expected)
				}
			}
			if mode == "trust" && !strings.Contains(lines, "outside Unreal Operations") {
				t.Fatal("trust described as verified policy safety")
			}
			for _, format := range []string{"json", "markdown"} {
				b, err := Encode(r, format)
				if err != nil || strings.Contains(string(b), "sensitive") {
					t.Fatal("analysis exported policy/credential/transcript contents", err)
				}
				if format == "json" && !strings.Contains(string(b), `"managedPolicyMode": "`+want+`"`) {
					t.Fatal("export omitted explicit boundary")
				}
			}
			// A new fork's configuration supplies its own trust boundary.
			a.Apply(host.HistoryItem{Data: sessionstore.Fork{}})
			if a.Snapshot(time.Now(), true, true, nil, nil).ManagedPolicyMode != "" {
				t.Fatal("fork retained inherited trust metadata")
			}
		})
	}
}

func TestAgentsContextTimelineErrorsKnownAndUnobserved(t *testing.T) {
	a, now := analysisFixture(t)
	rows := []viewer.Row{{ID: "test", Elapsed: viewer.Duration{Known: true, Value: time.Minute}}, {ID: "child-1", ParentID: "test", Cursor: 3, Runtime: viewer.RuntimeStopped, Finish: &viewer.Finish{Status: "failure", Summary: "child-body-sensitive", RecordedAt: now}, Usage: viewer.Usage{Known: true, Input: 42, Output: 4}, Operations: []viewer.OperationRow{{ID: "child-op"}}}, {ID: "child-2", ParentID: "test", Runtime: viewer.RuntimeUnknown, ParentOperationStatus: operation.StatusAwaiting}}
	r := a.Snapshot(now, true, false, rows, nil)
	if len(r.Agents) != 2 || !r.Agents[0].Observed || r.Agents[0].Operations == nil || *r.Agents[0].Operations != 1 || r.Agents[1].Observed || r.Agents[1].Operations != nil || r.Agents[1].Usage.Known {
		t.Fatal("inferred unobserved child", r.Agents)
	}
	if !r.Elapsed.Known || r.Errors.Provider != 1 || r.Errors.Tools != 2 || r.Errors.Children != 1 || !r.Errors.CrashesKnown || r.Errors.Crashes != 0 {
		t.Fatal("errors counted UI-only state", r.Errors)
	}
	if r.AgentCounts.Discovered != 2 || r.AgentCounts.Observed != 1 || r.AgentCounts.Failed != 1 || r.AgentCounts.UnknownFinish != 1 || r.AgentCounts.Success != 0 {
		t.Fatal("invented agent outcomes", r.AgentCounts)
	}
	if len(r.Context) != 1 || r.Context[0].Usage.Input != 100 || len(r.Timeline) == 0 {
		t.Fatal("missing observed context/timeline")
	}
	for _, view := range []string{"Agents", "Context", "Timeline", "Errors"} {
		s := strings.Join(Lines(r, view, true), "\n")
		if strings.Contains(s, "analysis unavailable") || strings.Contains(s, "child-body-sensitive") || strings.ContainsAny(s, "\x1b█") {
			t.Fatal(view, s)
		}
	}
	if s := strings.Join(Lines(r, "Context", false), "\n"); strings.Contains(s, "%") || !strings.Contains(s, "Latest known input") {
		t.Fatal("invented current context percentage", s)
	}
	for _, e := range r.Timeline {
		if e.At.IsZero() {
			t.Fatal("invented timestamp")
		}
	}
	before := r.Errors
	disconnected := a.Snapshot(now, false, true, rows, nil)
	if disconnected.Errors.Provider != before.Provider || disconnected.Errors.Tools != before.Tools || disconnected.Errors.Children != before.Children {
		t.Fatal("disconnect counted as canonical failure")
	}
	encoded, err := Encode(r, "JSON")
	if err != nil || strings.Contains(string(encoded), "child-body-sensitive") {
		t.Fatal("child result body exported", err)
	}
}

func TestContextRatioRequiresCompleteLatestUsageAndCatalogMetadata(t *testing.T) {
	a := New("s")
	choice := sessionstore.RuntimeSelection{Version: 1, Revision: 1, Provider: "openai-codex", Model: "m", Name: "M", Effort: llm.ReasoningEffortMedium, ContextWindow: 100}
	a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &choice}})
	a.Apply(host.HistoryItem{Data: session.Turn{ID: "t1", RuntimeRevision: 1}})
	a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{TurnID: "t1", Response: llm.Response{Usage: llm.Usage{InputTokens: 25}}}})
	known := a.Snapshot(time.Now(), true, false, nil, nil)
	if !strings.Contains(strings.Join(Lines(known, "Context", false), "\n"), "25.0%") {
		t.Fatal("catalog context metadata was not used")
	}
	partial := a.Snapshot(time.Now(), true, true, nil, nil)
	if strings.Contains(strings.Join(Lines(partial, "Context", false), "\n"), "%") {
		t.Fatal("incomplete history presented as current context ratio")
	}
	a.Apply(host.HistoryItem{Data: session.Turn{ID: "t2", RuntimeRevision: 1}})
	unknown := a.Snapshot(time.Now(), true, false, nil, nil)
	if strings.Contains(strings.Join(Lines(unknown, "Context", false), "\n"), "%") {
		t.Fatal("older usage presented as latest request usage")
	}
}

func TestAnalysisDisplayBoundsAndUnappliedRevisionStayPartial(t *testing.T) {
	a := New("s")
	choice := sessionstore.RuntimeSelection{Version: 1, Revision: 1, Provider: "openai-codex", Model: "m", Name: "M", Effort: llm.ReasoningEffortMedium}
	a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Kind: sessionstore.HostRuntimeSelection, Selection: &choice}})
	for i := range 4100 {
		a.Apply(host.HistoryItem{Data: session.Turn{ID: session.TurnID(fmt.Sprint(i)), RuntimeRevision: 1}})
	}
	r := a.Snapshot(time.Now(), true, false, nil, nil)
	if !r.Partial || len(r.Turns) != 4096 || r.Turns[0].Selection != nil || r.Selection != nil || r.Pending == nil {
		t.Fatal("bounded/incomplete history inferred an applied selection")
	}
}

func TestRecentErrorsRetainNewestCanonicalTimestampsWithExactCounts(t *testing.T) {
	a, now := analysisFixture(t)
	a.Apply(host.HistoryItem{RecordedAt: now, Data: session.Turn{ID: "errors", RuntimeRevision: 1}})
	for i := range 150 {
		id := fmt.Sprintf("tool-%03d", i)
		a.Apply(host.HistoryItem{RecordedAt: now, Data: sessionstore.ModelResponse{TurnID: "errors", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: id, Name: id}}}}}})
		now = now.Add(time.Second)
		a.Apply(host.HistoryItem{RecordedAt: now, Data: sessionstore.ToolCallStatus{TurnID: "errors", CallID: id, Status: tool.CallStatus{Error: "body-sensitive"}}})
		now = now.Add(time.Second)
	}
	r := a.Snapshot(now, true, false, nil, nil)
	if r.Errors.Tools != 152 || len(r.Errors.Recent) != 128 || r.Errors.Recent[127].Name != "tool-149 failed" || !r.Partial {
		t.Fatal("recent errors lost exact counts or newest observation")
	}
	for i := 1; i < len(r.Errors.Recent); i++ {
		if r.Errors.Recent[i].At.Before(r.Errors.Recent[i-1].At) {
			t.Fatal("map iteration determined error chronology")
		}
	}
}
func TestAnalysisCanonicalSettingsAndCrashMetadata(t *testing.T) {
	a, now := analysisFixture(t)
	settings, err := json.Marshal(inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: llm.ReasoningEffortLow}})
	if err != nil {
		t.Fatal(err)
	}
	a.Apply(host.HistoryItem{RecordedAt: now, Data: inbox.Input{Kind: inbox.InputControl, Payload: settings}})
	a.Apply(host.HistoryItem{RecordedAt: now, Data: session.Turn{ID: "t3", RuntimeRevision: 1}})
	a.Apply(host.HistoryItem{RecordedAt: now, Data: inbox.Input{Kind: inbox.InputCrash, Payload: []byte(`"crash-secret-sensitive"`)}})
	r := a.Snapshot(now, true, false, nil, nil)
	if r.Turns[1].Selection.Effort != "high" || r.Turns[2].Selection.Effort != "low" || r.Selection.Effort != "low" || !r.Errors.CrashesKnown || r.Errors.Crashes != 1 {
		t.Fatal("canonical settings/crash not preserved", r.Selection, r.Errors)
	}
	data, err := Encode(r, "JSON")
	if err != nil || strings.Contains(string(data), "crash-secret-sensitive") {
		t.Fatal("crash body exported", err)
	}
}
func TestOverviewUsageTurnsToolsAndMetadataIsolation(t *testing.T) {
	a, now := analysisFixture(t)
	r := a.Snapshot(now, true, false, nil, nil)
	if !r.Usage.Known || !r.Usage.Partial || r.Usage.Input != 100 || r.Usage.Responses != 2 || r.Usage.CacheWriteInput != 10 {
		t.Fatal(r.Usage)
	}
	if len(r.Turns) != 2 || r.Turns[0].Selection.Model != "model-a" || r.Turns[0].Selection.Effort != "medium" || r.Turns[1].Selection.Model != "model-b" || r.Turns[1].Selection.Effort != "high" {
		t.Fatal(r.Turns)
	}
	if r.Tools.Calls != 3 || r.Tools.Completed != 1 || r.Tools.Failed != 1 || r.Tools.Canceled != 1 || r.ByTool[1].Name != "read" || r.ByTool[1].Calls != 2 {
		t.Fatal(r.Tools, r.ByTool)
	}
	for _, view := range []string{"Overview", "Usage", "Turns", "Tools"} {
		lines := strings.Join(Lines(r, view, false), "\n")
		if lines == "" || strings.Contains(lines, "analysis unavailable") {
			t.Fatal(view, lines)
		}
	}
	if !strings.Contains(strings.Join(Lines(r, "Usage", false), "\n"), "~100") {
		t.Fatal("partial usage rendered as exact")
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"authorization-sensitive", "prompt-sensitive", "input-sensitive", "agent-sensitive", "tool-args-sensitive", "error-credential-sensitive", "operation-sensitive", "provider-credential-sensitive"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("report retained body or credentials")
		}
	}
	empty := New("empty").Snapshot(now, true, false, nil, nil)
	if empty.Usage.Known || !strings.Contains(strings.Join(Lines(empty, "Usage", false), "\n"), "unknown") {
		t.Fatal("invented zero usage")
	}
	partial := a.Snapshot(now, false, true, nil, nil)
	if !partial.Partial || !partial.Resync {
		t.Fatal("resync lost partial marker")
	}
}
