package testsession

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type settledLog struct {
	tb  testing.TB
	out bytes.Buffer
	seq sessionstore.Sequence
	at  time.Time
}

func (l *settledLog) line(kind string, data any) {
	l.tb.Helper()
	raw, err := json.Marshal(struct {
		Type string `json:"type"`
		Data any    `json:"data"`
	}{kind, data})
	if err != nil {
		l.tb.Fatal(err)
	}
	l.out.Write(raw)
	l.out.WriteByte('\n')
}
func (l *settledLog) item(kind sessionstore.ItemKind, data any) {
	l.seq++
	l.at = l.at.Add(time.Second)
	l.line("item", struct{ Item sessionstore.Item }{sessionstore.Item{Sequence: l.seq, RecordedAt: l.at, Kind: kind, Data: data}})
}
func (l *settledLog) header(id session.ID, config any) {
	l.line("session", struct {
		Version int
		Session session.Session
	}{3, session.Session{ID: id, CreatedAt: l.at}})
	raw, err := json.Marshal(config)
	if err != nil {
		l.tb.Fatal(err)
	}
	l.item(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: raw})
}
func (l *settledLog) write(directory string, id session.ID) {
	l.tb.Helper()
	if err := os.WriteFile(filepath.Join(directory, string(id)+".session.jsonl"), l.out.Bytes(), 0600); err != nil {
		l.tb.Fatal(err)
	}
}

// WriteSettledChildren writes only synthetic canonical logs, with successful,
// failed and canceled children whose end reports have reached their parent.
// It calls no model/API and allocates only one child log at a time.
func WriteSettledChildren(tb testing.TB, directory string, count, bodyBytes int) []session.ID {
	tb.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		tb.Fatal(err)
	}
	at := time.Unix(1700000000, 0).UTC()
	parent := &settledLog{tb: tb, at: at}
	parent.header("main", map[string]any{})
	var ids []session.ID
	for i := range count {
		opID := operation.ID(fmt.Sprintf("spawn-%04d", i))
		id := subagent.ChildID("main", opID)
		ids = append(ids, id)
		cfg := subagent.ChildConfig{Version: 1, ParentID: "main", ChildID: id, OperationID: opID, SessionDirectory: directory, Workspace: directory, Runtime: []byte(`{}`), ReadyID: inbox.ID("ready:" + string(opID)), Task: "synthetic child", Policy: permission.Config{Tools: []string{"Finish"}}}
		child := &settledLog{tb: tb, at: at}
		child.header(id, cfg)
		child.item(sessionstore.ItemTurn, session.Turn{ID: "child-turn", Type: session.TurnRegular})
		child.item(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "child-turn", Response: llm.Response{Model: "synthetic", Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: strings.Repeat("x", bodyBytes)}}}, Usage: llm.Usage{InputTokens: int64(i + 100), OutputTokens: 50}}})
		status := []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled}[i%3]
		var finish *sessionstore.FinishRecord
		if status != operation.StatusCanceled {
			finish = &sessionstore.FinishRecord{Version: 1, ModelTurnID: "child-turn", Result: sessionstore.FinishResult{Status: string(status), Summary: "synthetic report", Tests: []string{"synthetic focused test: PASS"}}}
			child.item(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: finish})
		} else {
			child.item(sessionstore.ItemHostRecord, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostEnded, End: &sessionstore.EndRecord{EndedBy: "host_restart", EndedAt: child.at}})
		}
		child.write(directory, id)
		spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: "main", ChildID: id, Template: "worker", Text: cfg.Task, Configuration: &subagent.Template{Workspace: directory, Runtime: cfg.Runtime, Policy: cfg.Policy}, Notify: true})
		if err != nil {
			tb.Fatal(err)
		}
		raw, _ := json.Marshal(spec)
		payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: string(opID), ToolName: "SubagentStart", Spec: raw}})
		parent.item(sessionstore.ItemInput, inbox.Input{ID: inbox.ID(opID), Kind: inbox.InputControl, Payload: payload})
		state, err := operation.DecodeRemoteJobState(operation.Operation{Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength})
		if err != nil {
			tb.Fatal(err)
		}
		state.Handle, _ = json.Marshal(subagent.Handle{Version: 1, ParentID: "main", ChildID: id, OperationID: opID, Finish: finish})
		encoded, _ := json.Marshal(state)
		parent.line("operation", struct{ Operation operation.Operation }{operation.Operation{ID: opID, ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: encoded, Status: status, MaxOutputLength: spec.MaxOutputLength}})
		peer, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(id), Target: "main", Handle: string(opID), Kind: "ended", Text: "Child " + string(opID) + " ended: " + string(status)})
		parent.item(sessionstore.ItemInput, inbox.Input{ID: inbox.ID("ended:" + string(opID)), Kind: inbox.InputPeer, Payload: peer})
	}
	parent.item(sessionstore.ItemTurn, session.Turn{ID: "tail-turn", Type: session.TurnRegular})
	parent.item(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "tail-turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "all synthetic reports received"}}}}})
	parent.write(directory, "main")
	return ids
}
