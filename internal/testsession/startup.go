// Package testsession provides synthetic canonical logs for startup measurements.
package testsession

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// Synthetic canonical logs only: no credentials, providers or real Sessions.
// Main: 6000+ items / approximately 10 MB; 200 children with 150 items each.
func StartupLogs(tb testing.TB, directory string, configuration any) map[string][]byte {
	tb.Helper()
	logs := make(map[string][]byte)
	encode := func(id session.ID, exchanges, body int, config any, children bool) []byte {
		var out bytes.Buffer
		line := func(kind string, data any) {
			b, err := json.Marshal(struct {
				Type string `json:"type"`
				Data any    `json:"data"`
			}{kind, data})
			if err != nil {
				tb.Fatal(err)
			}
			out.Write(b)
			out.WriteByte('\n')
		}
		at := time.Unix(1700000000, 0).UTC()
		line("session", struct {
			Version int
			Session session.Session
		}{3, session.Session{ID: id, CreatedAt: at}})
		seq := sessionstore.Sequence(0)
		item := func(kind sessionstore.ItemKind, data any) {
			seq++
			line("item", struct{ Item sessionstore.Item }{sessionstore.Item{Sequence: seq, RecordedAt: at, Kind: kind, Data: data}})
		}
		raw, err := json.Marshal(config)
		if err != nil {
			tb.Fatal(err)
		}
		item(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: raw})
		var previous session.TurnID
		for i := range exchanges {
			payload, _ := json.Marshal(fmt.Sprintf("synthetic input %d", i))
			item(sessionstore.ItemInput, inbox.Input{ID: inbox.ID(fmt.Sprintf("i-%d", i)), Kind: inbox.InputExternal, Payload: payload})
			tid := session.TurnID(fmt.Sprintf("t-%d", i))
			item(sessionstore.ItemTurn, session.Turn{ID: tid, PreviousTurnID: previous})
			item(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: tid, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: fmt.Sprintf("startup-entry-%04d ", i) + strings.Repeat("x", body)}}}}})
			previous = tid
		}
		if children {
			for i := range 200 {
				opID := operation.ID(fmt.Sprintf("spawn-%03d", i))
				spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: id, Template: "default", Text: "synthetic child", Configuration: &subagent.Template{Workspace: directory, Runtime: []byte(`{}`)}})
				if err != nil {
					tb.Fatal(err)
				}
				raw, _ := json.Marshal(spec)
				payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(opID), ToolName: "SubagentStart", Spec: raw}})
				item(sessionstore.ItemInput, inbox.Input{ID: inbox.ID(opID), Kind: inbox.InputControl, Payload: payload})
				line("operation", struct{ Operation operation.Operation }{operation.Operation{ID: opID, ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusCompleted}})
			}
			item(sessionstore.ItemInput, inbox.Input{ID: "tail-input", Kind: inbox.InputExternal, Payload: []byte(`"tail input"`)})
			item(sessionstore.ItemTurn, session.Turn{ID: "tail-turn", PreviousTurnID: previous})
			item(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "tail-turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "startup-entry-1999"}}}}})
		}
		return out.Bytes()
	}
	logs["main.session.jsonl"] = encode("main", 2000, 4500, configuration, true)
	for i := range 200 {
		opID := operation.ID(fmt.Sprintf("spawn-%03d", i))
		id := subagent.ChildID("main", opID)
		config := subagent.ChildConfig{Version: 1, ParentID: "main", ChildID: id, OperationID: opID, SessionDirectory: directory, Workspace: directory, Runtime: []byte(`{}`), ReadyID: inbox.ID("ready:" + string(opID)), Task: "synthetic child"}
		logs[string(id)+".session.jsonl"] = encode(id, 50, 1000, config, false)
	}
	return logs
}
