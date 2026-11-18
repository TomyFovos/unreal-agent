package host

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestHostCheckpointSendsSameRequestAsFullReplay(t *testing.T) {
	canonical, midTurn, afterFailure := checkpointConversation(t)
	var full []byte
	for _, tc := range []struct {
		name string
		data []byte
	}{{"full replay", nil}, {"checkpoint before tool boundary", midTurn}, {"checkpoint after failed turn", afterFailure}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "parent.session.jsonl")
			checkHostRestore(t, os.WriteFile(logPath, canonical, 0o600))
			if tc.data != nil {
				checkHostRestore(t, os.WriteFile(filepath.Join(dir, "parent.restore-checkpoint"), tc.data, 0o600))
			}
			requests := make(chan []byte, 1)
			client := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: restoreRoundTripper(func(r *http.Request) (*http.Response, error) {
				r.Body.Close()
				const completed = `{"type":"response.completed","response":{"id":"response","object":"response","status":"completed","output":[]}}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + completed + "\n\n"))}, nil
			})})
			adapter, err := responsesapi.NewAdapter(client, responsesapi.Config{
				Endpoint: "https://restore.test/responses",
				Trace: func(exchange responsesapi.Exchange) {
					requests <- bytes.Clone(exchange.RequestBody)
				},
			})
			checkHostRestore(t, err)
			children := &restoreChildManager{active: make(chan operation.Operation, 4), updates: make(chan operation.Operation)}
			h, err := New(t.Context(), Config{Directory: dir, PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
				builder := contextbuilder.NewBuilder()
				engine, err := contextbuilder.EnableCompaction(builder, contextengine.Config{InputBudget: 12000}, contextengine.Constraints{})
				if err != nil {
					return Runtime{}, err
				}
				builder.SetModel(llm.Model{ID: "fixture"})
				builder.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("openai-codex")
				selected := permission.ControllerFromContext(ctx).Snapshot().SelectedMode
				builder.(interface{ SetPermissionPrompt(string) }).SetPermissionPrompt("Workspace selection is " + string(selected))
				return Runtime{
					Builder: builder, LLM: adapter, ContextEngine: engine, Operations: children,
					Tools: tool.NewRegistry(tool.StaticTranslators{}), Close: client.Close,
					RetainOperation: func(_ context.Context, op operation.Operation) (bool, error) {
						return op.ID == "live-child" && op.Status == operation.StatusAwaiting, nil
					},
				}, nil
			}})
			checkHostRestore(t, err)
			t.Cleanup(func() { h.Close() })
			// The workspace choice can change after a Session checkpoint exists.
			h.savePermissionMode(permission.FullAccess)
			s, err := h.Resume(t.Context(), Options{ID: "parent", Initial: []inbox.Input{input("next", "review this next")}})
			checkHostRestore(t, err)
			var request []byte
			select {
			case request = <-requests:
			case <-s.Done():
				t.Fatalf("resume ended before inference: %v", s.Wait(t.Context()))
			case <-timeout(t).Done():
				t.Fatal("resume did not send a request")
			}
			if tc.data == nil {
				full = request
			} else if !bytes.Equal(request, full) {
				t.Fatalf("request changed across checkpoint restore\nfull: %s\n checkpoint: %s", full, request)
			}
			for _, marker := range []string{"summary of old work", "peer arrived mid-turn", "pending failed request", "review this next", "Workspace selection is full_access", "opaque-native-reasoning"} {
				if bytes.Count(request, []byte(marker)) != 1 {
					t.Fatalf("request should include %q once: %s", marker, request)
				}
			}
			for _, excluded := range []string{"pre-compaction text", "capped Claude text"} {
				if bytes.Contains(request, []byte(excluded)) {
					t.Fatalf("excluded context %q was sent after resume", excluded)
				}
			}
			view, err := s.Inspect(s.StartAfter, 1)
			checkHostRestore(t, err)
			if view.Permissions.SelectedMode != permission.FullAccess || view.Permissions.Mode != permission.FullAccess {
				t.Fatalf("workspace choice was lost: %+v", view.Permissions)
			}
			select {
			case child := <-children.active:
				if child.ID != "live-child" || child.Status != operation.StatusAwaiting || !bytes.Contains(child.State, []byte("original-handle")) {
					t.Fatal("resume changed the live child's Operation", child)
				}
			default:
				t.Fatal("resume did not reattach the live child")
			}
			checkHostRestore(t, h.Close())
			log, err := os.ReadFile(logPath)
			checkHostRestore(t, err)
			if !bytes.HasPrefix(log, canonical) {
				t.Fatal("Host resume changed the original canonical history")
			}
		})
	}
}

func checkpointConversation(t *testing.T) (canonical, midTurn, afterFailure []byte) {
	t.Helper()
	dir := t.TempDir()
	const id = session.ID("parent")
	ctx := t.Context()
	store, err := localfile.New(dir)
	checkHostRestore(t, err)
	lease, err := store.AcquireWriter(id)
	checkHostRestore(t, err)
	defer lease.Close()
	_, err = store.Create(ctx, id)
	checkHostRestore(t, err)
	view, err := store.OpenCheckpointedRestoreView(ctx, id)
	checkHostRestore(t, err)
	_, _, err = view.Take(ctx, id)
	checkHostRestore(t, err)
	save := func() []byte {
		t.Helper()
		checkHostRestore(t, store.SaveRestoreCheckpoint(id))
		data, err := os.ReadFile(filepath.Join(dir, "parent.restore-checkpoint"))
		checkHostRestore(t, err)
		return data
	}
	appendTurn := func(turn session.Turn) {
		t.Helper()
		checkHostRestore(t, store.AppendTurn(ctx, id, turn))
	}
	appendInput := func(in inbox.Input) {
		t.Helper()
		checkHostRestore(t, store.AppendInput(ctx, id, in))
	}
	appendResponse := func(turn session.TurnID, text string, extra ...llm.Item) {
		t.Helper()
		out := append(extra, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}})
		checkHostRestore(t, store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{TurnID: turn,
			Response: llm.Response{ID: string(turn), Output: out, Usage: llm.Usage{InputTokens: 82, Raw: []byte(`{"attribution":{"items":{"request":82}}}`)}}}))
	}
	contextRecord := func(r sessionstore.ContextRecord) {
		t.Helper()
		checkHostRestore(t, store.AppendHostRecord(ctx, id, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &r}))
	}
	appendInput(input("old", "pre-compaction text"))
	appendTurn(session.Turn{ID: "old", Type: session.TurnRegular})
	appendResponse("old", "earlier answer")
	appendTurn(session.Turn{ID: "compact", Type: session.TurnCompaction, PreviousTurnID: "old"})
	appendResponse("compact", "summary of old work")
	contextRecord(sessionstore.ContextRecord{Kind: "summary", Through: "unit-00000000000000000002", Text: "summary of old work"})
	contextRecord(sessionstore.ContextRecord{Kind: "peer_format", Text: "quoted-v1"})

	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "subagent", Version: 1, Data: []byte(`{"Action":"start"}`)})
	checkHostRestore(t, err)
	raw, err := json.Marshal(spec)
	checkHostRestore(t, err)
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{OperationID: "live-child", ToolName: "SubagentStart", Spec: raw}})
	checkHostRestore(t, err)
	intent := inbox.Input{ID: "start", Kind: inbox.InputControl, Payload: payload}
	appendInput(intent)
	child, err := sessionstore.DecodeOperationIntent(intent)
	checkHostRestore(t, err)
	checkHostRestore(t, store.SaveOperation(ctx, id, child))
	appendInput(input("task", "work on this"))
	appendTurn(session.Turn{ID: "origin", Type: session.TurnRegular, PreviousTurnID: "compact"})
	peer, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "parent", Handle: "live-child", Kind: "message", Text: "peer arrived mid-turn"})
	checkHostRestore(t, err)
	appendInput(inbox.Input{ID: "peer", Kind: inbox.InputPeer, Deferred: true, Payload: peer})
	midTurn = save()

	appendTurn(session.Turn{ID: "continuation", Type: session.TurnRegular, PreviousTurnID: "origin", ToolContinuation: "origin", InputWatermark: 3, ExternalInputWatermark: 2, BatchInputs: true})
	appendResponse("continuation", "peer acknowledged", llm.Item{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"id":"reasoning","type":"reasoning","summary":[],"encrypted_content":"opaque-native-reasoning"}`)}})
	child.Status = operation.StatusAwaiting
	state, err := operation.DecodeRemoteJobState(child)
	checkHostRestore(t, err)
	state.Handle = []byte(`{"id":"original-handle"}`)
	child.State, err = json.Marshal(state)
	checkHostRestore(t, err)
	checkHostRestore(t, store.SaveOperation(ctx, id, child))
	appendInput(input("pending", "pending failed request"))
	appendTurn(session.Turn{ID: "failed", Type: session.TurnRegular, PreviousTurnID: "continuation"})
	checkHostRestore(t, store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{TurnID: "failed", AdapterError: true, Response: llm.Response{
		ID: "failed", Stop: llm.StopMaxOutputTokens, Failure: &llm.Failure{Code: "turn_failed", Message: "synthetic failure"},
		Output: []llm.Item{{Type: llm.ItemMessage, ProviderID: "claude-code/capped/1", OutputLimit: true, Data: llm.Message{Role: llm.RoleAssistant, Text: "capped Claude text"}}},
	}}))
	checkHostRestore(t, store.AppendHostRecord(ctx, id, sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostActivity,
		Activity: &llm.ActivityRecord{Kind: llm.ActivityModelFailure, ID: "failed", Reason: "synthetic failure"}}))
	afterFailure = save()
	canonical, err = os.ReadFile(filepath.Join(dir, "parent.session.jsonl"))
	checkHostRestore(t, err)
	return canonical, midTurn, afterFailure
}

type restoreRoundTripper func(*http.Request) (*http.Response, error)

func (f restoreRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type restoreChildManager struct {
	active, updates chan operation.Operation
}

func (m *restoreChildManager) Add(op operation.Operation) error    { m.active <- op; return nil }
func (m *restoreChildManager) Cancel(operation.ID, string) error   { return nil }
func (m *restoreChildManager) Updates() <-chan operation.Operation { return m.updates }

func checkHostRestore(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
