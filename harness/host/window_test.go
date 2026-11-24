package host

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

const oldWindowInput = "Original wording of the previous request."
const windowQuestion = "【要確認】 Which target should I use?"

func seedWindowHistory(t *testing.T, dir string) (*localfile.Store, string, record.Locator) {
	t.Helper()
	ctx := t.Context()
	store, err := localfile.New(dir)
	checkHostRestore(t, err)
	_, err = store.Create(ctx, "window")
	checkHostRestore(t, err)
	old := oldWindowInput + strings.Repeat(" Previous work.", 1300)
	seq, err := store.AppendInputRecorded(ctx, "window", input("old", old))
	checkHostRestore(t, err)
	checkHostRestore(t, store.AppendTurn(ctx, "window", session.Turn{ID: "old", Type: session.TurnRegular, ContextStrategy: contextengine.WindowThreeLayer}))
	checkHostRestore(t, store.AppendModelResponse(ctx, "window", sessionstore.ModelResponse{
		TurnID: "old", Response: llm.Response{Stop: llm.StopComplete, Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: windowQuestion}},
		}},
	}))
	return store, old, record.Input("window", uint64(seq), record.UserMessage)
}

func windowBuilder(registry tool.Registry, config contextengine.Config) (contextbuilder.Builder, *contextengine.Engine, error) {
	b := contextbuilder.NewBuilder()
	engine, err := contextbuilder.EnableWindow(b, config, contextengine.Constraints{})
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("openai-codex")
	b.SetModel(llm.Model{ID: "fixture", ReasoningEffort: llm.ReasoningEffortMedium})
	for _, d := range registry.StaticDefinitions() {
		b.AddTool(d.Tool)
	}
	return b, engine, err
}

func TestHostRebuildsOversizedAutonomousTailWithOrWithoutCheckpoint(t *testing.T) {
	const id session.ID = "long-tail"
	const active = "  Continue the release review.\r\n\tこの原文を保持する。\n"
	const threshold = 8_000
	ctx, seed := t.Context(), t.TempDir()
	store, err := localfile.New(seed)
	checkHostRestore(t, err)
	lease, err := store.AcquireWriter(id)
	checkHostRestore(t, err)
	t.Cleanup(func() { lease.Close() })
	_, err = store.Create(ctx, id)
	checkHostRestore(t, err)
	seq, err := store.AppendInputRecorded(ctx, id, input("current-request", active))
	checkHostRestore(t, err)
	activeLocator := record.Input(string(id), uint64(seq), record.UserMessage)
	large := strings.Repeat("autonomous review already completed\n", 1_500)
	checkHostRestore(t, store.AppendTurn(ctx, id, session.Turn{ID: "original", Type: session.TurnRegular}))
	checkHostRestore(t, store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{
		TurnID: "original", Response: llm.Response{Stop: llm.StopComplete, Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "The review is in progress."}},
		}},
	}))
	checkHostRestore(t, store.AppendTurn(ctx, id, session.Turn{ID: "autonomous", PreviousTurnID: "original", Type: session.TurnRegular}))
	checkHostRestore(t, store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{
		TurnID: "autonomous", Response: llm.Response{Stop: llm.StopComplete, Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: large}},
		}},
	}))
	largeLocator := record.Address{Session: string(id), Sequence: uint64(seq + 4), Kind: record.AssistantMessage}.Locator()
	peer, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "worker", Target: string(id), Handle: "worker",
		Kind: "message", Text: "The child has new results to review."})
	checkHostRestore(t, err)
	checkHostRestore(t, store.AppendInput(ctx, id, inbox.Input{ID: "waking-peer", Kind: inbox.InputPeer, Payload: peer}))

	view, err := store.OpenCheckpointedRestoreView(ctx, id)
	checkHostRestore(t, err)
	_, _, err = view.Take(ctx, id)
	checkHostRestore(t, err)
	checkHostRestore(t, store.SaveRestoreCheckpoint(id))
	logName, checkpointName := string(id)+".session.jsonl", string(id)+".restore-checkpoint"
	canonical, err := os.ReadFile(filepath.Join(seed, logName))
	checkHostRestore(t, err)
	checkpoint, err := os.ReadFile(filepath.Join(seed, checkpointName))
	checkHostRestore(t, err)

	var full []byte
	for _, mode := range []string{"full replay", "checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			ctx, dir := t.Context(), t.TempDir()
			checkHostRestore(t, os.WriteFile(filepath.Join(dir, logName), canonical, 0600))
			if mode == "checkpoint" {
				checkHostRestore(t, os.WriteFile(filepath.Join(dir, checkpointName), checkpoint, 0600))
			}
			requests := make(chan llm.Request, 1)
			h, err := New(ctx, Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
				registry := tool.NewRegistry(tool.StaticTranslators{})
				b, engine, err := windowBuilder(registry, contextengine.Config{WindowThresholdTokens: threshold, L2TargetTokens: threshold / 2})
				return Runtime{Builder: b, ContextEngine: engine, Operations: operation.NewLocalOperationManager(ctx), Tools: registry,
					LLM: modelFunc(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
						requests <- r
						return llm.Response{Stop: llm.StopComplete}, nil
					})}, err
			}})
			checkHostRestore(t, err)
			t.Cleanup(func() { h.Close() })
			s, err := h.Resume(ctx, Options{ID: id})
			checkHostRestore(t, err)
			var req llm.Request
			select {
			case req = <-requests:
			case <-s.Done():
				t.Fatalf("Host stopped before the first window: %v", s.Wait(ctx))
			case <-timeout(t).Done():
				t.Fatal("no window was sent for the pending peer input")
			}
			tokens := (contextengine.TokenEstimator{}).Request(req)
			if !req.PublicHistory || len(req.Input) != 3 || tokens >= threshold {
				t.Fatalf("first window: %d items, %d tokens", len(req.Input), tokens)
			}
			prefix, body, _ := strings.Cut(contextengine.Text(req.Input[1]), "\n")
			var state contextstate.State
			checkHostRestore(t, json.Unmarshal([]byte(body), &state))
			got := state.Requests.Current["current-request"]
			if got.Text != active || got.Locator != activeLocator || got.Status != contextstate.RequestActive ||
				!strings.Contains(prefix, "Earlier conversation is in L3.") || !strings.Contains(prefix, "rec:long-tail/1 through rec:long-tail/5") ||
				!strings.Contains(contextengine.Text(req.Input[2]), "The child has new results") {
				t.Fatalf("L2 request, L3 range or queued peer is missing: %q %+v", prefix, got)
			}
			page, err := s.RecordShow(ctx, largeLocator, int64(len(large)))
			checkHostRestore(t, err)
			if string(page.Bytes) != large {
				t.Fatal("record show did not return the displaced conversation")
			}
			wire, err := json.Marshal(req, json.Deterministic(true))
			checkHostRestore(t, err)
			if mode == "full replay" {
				full = wire
			} else if !bytes.Equal(wire, full) {
				t.Fatal("first upgraded window changed across checkpoint replay")
			}
		})
	}
}

type liveWindowChild struct {
	*operation.LocalOperationManager
	reattached chan operation.Operation
}

func (m liveWindowChild) Add(op operation.Operation) error {
	if op.ID == "live-child" {
		m.reattached <- op
		return nil
	}
	return m.LocalOperationManager.Add(op)
}

func (m liveWindowChild) Cancel(id operation.ID, reason string) error {
	if id == "live-child" {
		return nil
	}
	return m.LocalOperationManager.Cancel(id, reason)
}

func TestHostWindowPreservesApprovalQuestionWaitAndRunningChild(t *testing.T) {
	testHostWindowPending(t, nil)
}

func TestHostWindowReadsDisplacedStateFromCanonicalLog(t *testing.T) {
	ctx, dir := t.Context(), t.TempDir()
	store, _, _ := seedWindowHistory(t, dir)
	const oldest = "Decision from the start of the review"
	var original record.Locator
	for i := range 40 {
		id := fmt.Sprintf("decision-%02d", i)
		text := "Keep the current approach"
		if i == 0 {
			text = oldest
		}
		event, err := contextstate.NewEvent(id, contextstate.DecisionRecorded, contextstate.Decision{
			ID: id, Decision: text, Reason: strings.Repeat("verified findings ", 80),
		})
		checkHostRestore(t, err)
		locator, err := store.AppendContextEvent(ctx, "window", event)
		checkHostRestore(t, err)
		if i == 0 {
			original = locator
		}
	}

	requests := make(chan llm.Request, 1)
	h, err := New(ctx, Config{Directory: dir, PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
		registry := tool.NewRegistry(tool.StaticTranslators{})
		b, engine, err := windowBuilder(registry, contextengine.Config{WindowThresholdTokens: 4_000, L2TargetTokens: 2_000})
		return Runtime{Builder: b, ContextEngine: engine, Tools: registry, Operations: operation.NewLocalOperationManager(ctx),
			LLM: modelFunc(func(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
				requests <- req
				return llm.Response{Stop: llm.StopComplete}, nil
			})}, err
	}})
	checkHostRestore(t, err)
	t.Cleanup(func() { h.Close() })
	s, err := h.Resume(ctx, Options{ID: "window", Policy: permission.Unrestricted()})
	checkHostRestore(t, err)
	_, err = s.Submit(timeout(t), s.Generation, input("continue", "Continue the review"))
	checkHostRestore(t, err)
	var request llm.Request
	select {
	case request = <-requests:
	case <-timeout(t).Done():
		t.Fatal("no request after the boundary")
	}
	locator := record.Address{Session: "window", Kind: record.ContextIndex, Section: "state"}.Locator()
	if len(request.Input) < 2 || !strings.Contains(contextengine.Text(request.Input[1]), string(locator)) ||
		strings.Contains(contextengine.Text(request.Input[1]), oldest) {
		t.Fatal("the bounded copy did not link to the displaced decisions")
	}
	page, err := s.RecordShow(ctx, locator, 0)
	checkHostRestore(t, err)
	var full contextstate.State
	checkHostRestore(t, json.Unmarshal(page.Bytes, &full))
	if got := full.Decisions["decision-00"]; len(full.Decisions) != 40 || got.Decision != oldest || got.Locator != original {
		t.Fatal("record show did not recover the full decisions and their original evidence")
	}
	matches, err := s.RecordFind(ctx, record.Query{Kind: record.SessionItem, Text: oldest})
	checkHostRestore(t, err)
	if len(matches) != 1 || matches[0].Locator != original {
		t.Fatalf("record find did not locate the displaced decision: %+v", matches)
	}
}

func testHostWindowPending(t *testing.T, fake *testclaude.Fixture) {
	t.Helper()
	ctx, dir := t.Context(), t.TempDir()
	store, old, oldLocator := seedWindowHistory(t, dir)
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{
		Type: "subagent", Version: 1,
		Data: []byte(`{"Action":"start","ParentID":"window","Template":"worker","Text":"fixture child"}`),
	})
	checkHostRestore(t, err)
	raw, err := json.Marshal(spec)
	checkHostRestore(t, err)
	payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation, Parameters: inbox.OperationIntent{
		OperationID: "live-child", ToolName: "SubagentStart", Spec: raw,
	}})
	checkHostRestore(t, err)
	intent := inbox.Input{ID: "child-start", Kind: inbox.InputControl, Payload: payload}
	checkHostRestore(t, store.AppendInput(ctx, "window", intent))
	child, err := sessionstore.DecodeOperationIntent(intent)
	checkHostRestore(t, err)
	job, err := operation.DecodeRemoteJobState(child)
	checkHostRestore(t, err)
	job.Handle = []byte(`{"Version":1}`)
	step, err := operation.UpdateRemoteJob(child, job, operation.StatusAwaiting)
	checkHostRestore(t, err)
	child = *step.Operation
	checkHostRestore(t, store.SaveOperation(ctx, "window", child))

	reattached := make(chan operation.Operation, 2)
	requests := make(chan llm.Request, 2)
	modelDirectory := t.TempDir()
	h, err := New(ctx, Config{Directory: dir, PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
		results := observed.NewResults()
		registry := tool.NewRegistry(tool.StaticTranslators{ClaudeCode: observed.New(results)})
		b, engine, err := windowBuilder(registry, contextengine.Config{WindowThresholdTokens: 2_000, L2TargetTokens: 100})
		if err != nil {
			return Runtime{}, err
		}
		var adapter llm.Adapter
		if fake != nil {
			adapter, err = claudeagent.NewClient(claudeagent.Config{Binary: fake.Binary, Directory: modelDirectory, Results: results, Namespace: dir})
			if err != nil {
				return Runtime{}, err
			}
			b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(claudeagent.ProviderID)
			b.(interface{ SetWindowPrompt(llm.WindowPrompt) }).SetWindowPrompt(claudeagent.WindowPromptLine)
		}
		manager := liveWindowChild{operation.NewLocalOperationManager(ctx), reattached}
		return Runtime{Builder: b, ContextEngine: engine, Tools: registry, Operations: manager,
			RetainOperation: func(_ context.Context, op operation.Operation) (bool, error) { return op.ID == child.ID, nil },
			LLM: modelFunc(func(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
				requests <- req
				if adapter != nil {
					return adapter.Respond(ctx, req, opt)
				}
				return llm.Response{Stop: llm.StopComplete}, nil
			})}, err
	}})
	checkHostRestore(t, err)
	t.Cleanup(func() { h.Close() })
	s, err := h.Resume(ctx, Options{ID: "window", Policy: permission.Unrestricted()})
	checkHostRestore(t, err)
	select {
	case op := <-reattached:
		if op.ID != child.ID || op.Status != operation.StatusAwaiting {
			t.Fatalf("running child changed on attach: %+v", op)
		}
	case <-timeout(t).Done():
		t.Fatal("running child was not reattached")
	}
	controller, err := s.PermissionControl(s.Generation)
	checkHostRestore(t, err)
	key, release := controller.AcquireControl()
	defer release()
	controlMode(t, controller, key, permission.Ask)
	_, err = s.SubmitOperation(timeout(t), s.Generation, "permission-input", "pending-approval", "Bash", humanShell(t, t.TempDir()))
	checkHostRestore(t, err)
	waitOperation(t, s, "pending-approval", func(op operation.Operation) bool {
		return op.Permission != nil && op.Permission.State == "pending"
	})
	appendEvent := func(id string, kind contextstate.EventKind, value any) record.Locator {
		t.Helper()
		event, err := contextstate.NewEvent(id, kind, value)
		checkHostRestore(t, err)
		loc, err := s.AppendContextEvent(ctx, event)
		checkHostRestore(t, err)
		return loc
	}
	wait := appendEvent("external-wait", contextstate.WaitChanged, contextstate.WaitUpdate{
		Wait: contextstate.Wait{ID: "report", Kind: contextstate.WaitExternal, Text: "waiting on the report"},
	})
	done := appendEvent("done-plan", contextstate.PlanChanged, contextstate.PlanStep{
		ID: "closed-step", Text: "retired plan body", Status: contextstate.PlanDone,
	})
	wantWaits := 2
	var retryAt time.Time
	if fake != nil {
		retryAt = time.Now().UTC().Add(time.Hour)
		checkHostRestore(t, s.RecordActivity(ctx, llm.ActivityRecord{Kind: "usage_limit_wait", RetryAt: retryAt}))
		wantWaits++
	}
	const verbatim = "  Keep going\r\n\t続き\n"
	_, err = s.Submit(timeout(t), s.Generation, input("continue", verbatim))
	checkHostRestore(t, err)
	var request llm.Request
	select {
	case request = <-requests:
	case <-timeout(t).Done():
		t.Fatal("no request while approvals and child work were pending")
	}
	if !request.PublicHistory || len(request.Input) != 3 || contextengine.Text(request.Input[2]) != verbatim {
		t.Fatalf("rebuilt request did not preserve the last input: %+v", request.Input)
	}
	_, stateBody, hasState := strings.Cut(contextengine.Text(request.Input[1]), "\n")
	var state contextstate.State
	checkHostRestore(t, json.Unmarshal([]byte(stateBody), &state))
	approval := state.Approvals.Current["pending-approval"]
	waiting := state.Waiting.Current["external:report"]
	currentChild := state.Children.Current[string(child.ID)]
	if !hasState || approval.Locator == "" || len(state.Waiting.Current["approval:pending-approval"].Sources) == 0 ||
		waiting.Kind != contextstate.WaitExternal || !bytes.Contains([]byte(stateBody), []byte(wait)) ||
		len(state.QuestionsToUser.Current) != 0 || state.QuestionsToUser.Processed.Count != 1 || len(state.Waiting.Current) != wantWaits ||
		currentChild.State != contextstate.ChildRunning || currentChild.LastEvent == "" ||
		state.Plan.Processed.Count != 1 || len(state.Plan.Steps) != 0 || strings.Contains(stateBody, "retired plan body") {
		t.Fatalf("unexpected state copy: %s", stateBody)
	}
	if fake != nil {
		if reqWait := state.Waiting.Current["usage_limit_wait:model"]; !reqWait.RetryAt.Equal(retryAt) || len(reqWait.Sources) == 0 {
			t.Fatalf("usage limit wait lost at boundary: %+v", reqWait)
		}
		checkClaudeWindowToolBatch(t, s, controller, key, fake, modelDirectory, stateBody, verbatim)
	}
	planIndex, err := s.RecordShow(ctx, state.Plan.Processed.Locator, 0)
	checkHostRestore(t, err)
	if !bytes.Contains(planIndex.Bytes, []byte(done)) {
		t.Fatal("settled plan lost its L3 evidence")
	}
	questions, err := s.RecordShow(ctx, state.QuestionsToUser.Processed.Locator, 0)
	checkHostRestore(t, err)
	var questionSources []record.Locator
	checkHostRestore(t, json.Unmarshal(questions.Bytes, &questionSources))
	foundQuestion := false
	for _, loc := range questionSources {
		addr, err := loc.Address()
		checkHostRestore(t, err)
		if addr.Kind == record.AssistantMessage {
			page, err := s.RecordShow(ctx, loc, 0)
			checkHostRestore(t, err)
			foundQuestion = foundQuestion || string(page.Bytes) == windowQuestion
		}
	}
	if !foundQuestion || strings.Contains(stateBody, windowQuestion) {
		t.Fatal("the earlier question did not move from L2 to L3 after the human input")
	}
	page, err := s.RecordShow(ctx, oldLocator, 0)
	checkHostRestore(t, err)
	matches, err := s.RecordFind(ctx, record.Query{Kind: record.UserMessage, Text: oldWindowInput})
	checkHostRestore(t, err)
	if string(page.Bytes) != old || len(matches) != 1 || matches[0].Locator != oldLocator || strings.Contains(stateBody, oldWindowInput) {
		t.Fatal("a dropped user message did not resolve to the original record")
	}
	state, err = s.ContextState(ctx)
	checkHostRestore(t, err)
	if state.Metrics.Rebuilds == nil || *state.Metrics.Rebuilds != 1 || state.Metrics.LastBoundary.WindowTokens == nil {
		t.Fatalf("rebuild did not update current metrics: %+v", state.Metrics)
	}
	checkHostRestore(t, controller.Decide(key, controller.Snapshot().Revision, "pending-approval", false))
	waitOperation(t, s, "pending-approval", func(op operation.Operation) bool { return op.Status == operation.StatusFailed })
	waitOperation(t, s, child.ID, func(op operation.Operation) bool { return op.Status == operation.StatusAwaiting })
}

func checkClaudeWindowToolBatch(t *testing.T, s *Session, controller *permission.Controller, key string, fake *testclaude.Fixture, directory, state, user string) {
	t.Helper()
	const nativeApproval = "claude:toolu_fake_1"
	ctx := timeout(t)
	for {
		snapshot, changed := controller.Watch()
		if slices.ContainsFunc(snapshot.Pending, func(p permission.Pending) bool { return p.ID == nativeApproval }) {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the Claude Code hooks did not reach an Ask approval")
		}
	}
	calls := fake.Calls(t)
	if len(calls) != 1 || !strings.Contains(calls[0].Input, state) || !strings.HasSuffix(calls[0].Input, user) {
		t.Fatal("the new conversation did not receive the pending state and verbatim user message")
	}
	path := filepath.Join(directory, "approved.txt")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the write ran before approval", err)
	}
	const peerText = "The running child has a progress update."
	peer := inbox.PeerMessage{Version: 1, Sender: "child-live", Target: string(s.ID), Handle: "live-child", Kind: "progress", Text: peerText}
	body, err := json.Marshal(peer)
	checkHostRestore(t, err)
	_, err = s.SubmitPeer(ctx, s.Generation, peer.Sender, peer.Handle, inbox.Input{ID: "progress", Kind: inbox.InputPeer, Payload: body})
	checkHostRestore(t, err)
	checkHostRestore(t, controller.Decide(key, controller.Snapshot().Revision, nativeApproval, true))
	for {
		busy, changed := s.ModelBusy()
		if !busy {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the conversation did not complete after approval")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "approved" {
		t.Fatalf("the original hook lost its approval: %q %v", data, err)
	}
	batches := fake.BatchContexts(t)
	if len(batches) != 1 || strings.Count(batches[0], peerText) != 1 || len(fake.Calls(t)) != 1 {
		t.Fatalf("peer update did not stay in the same PostToolBatch: %q", batches)
	}
}

type windowTextTool string

func (text windowTextTool) Translate(ctx tool.Context, _ llm.ToolCall) tool.CallStatus {
	spec, err := operation.NewValueSpec(jsontext.Value(text))
	if err != nil {
		return tool.ErrorStatus(err.Error(), 0)
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}
func (windowTextTool) TranslateResult(id string, status tool.CallStatus, ops []operation.Operation) (llm.ToolResult, error) {
	text := status.Error
	if len(ops) > 0 {
		value, err := operation.DecodeValue(ops[0])
		if err != nil {
			return llm.ToolResult{}, err
		}
		text = string(value)
	}
	return llm.ToolResult{CallID: id, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: text}}}, nil
}

func TestHostWindowCheckpointReplaysSameResponsesBytes(t *testing.T) {
	ctx, dir := t.Context(), t.TempDir()
	seedWindowHistory(t, dir)
	original := `"出力HEAD` + strings.Repeat(" middle locator fixture ", 85) + `TAIL終端"`
	const firstResponse = `{"id":"first","status":"completed","output":[{"id":"private","type":"reasoning","summary":[],"encrypted_content":"do-not-store"},{"id":"fc-1","type":"function_call","call_id":"call-1","name":"read","arguments":"{}","status":"completed"}],"usage":{"input_tokens":460,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":7},"output_tokens":8}}`
	const finishResponse = `{"id":"finish","status":"completed","output":[]}`
	open := func(dir string, responses ...string) (*Host, <-chan []byte) {
		t.Helper()
		requests := make(chan []byte, len(responses)+1)
		client := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: restoreRoundTripper(func(r *http.Request) (*http.Response, error) {
			r.Body.Close()
			response := finishResponse
			if len(responses) > 0 {
				response, responses = responses[0], responses[1:]
			}
			const event = "response.completed"
			body := "event: " + event + "\ndata: {\"type\":\"" + event + "\",\"response\":" + response + "}\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		adapter, err := responsesapi.NewAdapter(client, responsesapi.Config{
			Endpoint: "https://responses.test/responses",
			Trace:    func(exchange responsesapi.Exchange) { requests <- bytes.Clone(exchange.RequestBody) },
		})
		checkHostRestore(t, err)
		h, err := New(ctx, Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
			registry := tool.NewRegistry(tool.StaticTranslators{Read: windowTextTool(original)}, "read")
			b, engine, err := windowBuilder(registry, contextengine.Config{WindowThresholdTokens: 2_000, L2TargetTokens: 1_000, InputBudget: 4_000, ToolOutputExcerptChars: 28, CacheRewarmMinutes: 54})
			return Runtime{Builder: b, ContextEngine: engine, Tools: registry, Operations: operation.NewLocalOperationManager(ctx), LLM: adapter, Close: client.Close}, err
		}})
		checkHostRestore(t, err)
		t.Cleanup(func() { h.Close() })
		return h, requests
	}
	readRequest := func(s *Session, requests <-chan []byte, phase string) []byte {
		t.Helper()
		select {
		case req := <-requests:
			return req
		case <-s.Done():
			t.Fatalf("Session ended before %s: %v", phase, s.Wait(ctx))
			return nil
		case <-timeout(t).Done():
			view, _ := s.Inspect(s.StartAfter, 100)
			t.Fatalf("no Responses request during %s: %s %+v", phase, view.Failure, view.HistoryState)
			return nil
		}
	}
	h, requests := open(dir, firstResponse, finishResponse)
	s, err := h.Resume(ctx, Options{ID: "window", Policy: permission.Unrestricted(), Initial: []inbox.Input{input("continuation", "Continue from the last byte.\r\n")}})
	checkHostRestore(t, err)
	first, withTool := readRequest(s, requests, "rebuild"), readRequest(s, requests, "tool continuation")
	var firstWire, secondWire struct {
		Input []jsontext.Value `json:"input"`
	}
	checkHostRestore(t, json.Unmarshal(first, &firstWire))
	checkHostRestore(t, json.Unmarshal(withTool, &secondWire))
	if len(firstWire.Input) != 3 || len(secondWire.Input) != 5 || bytes.Contains(first, []byte("reasoning.encrypted_content")) ||
		bytes.Contains(withTool, []byte(`"id":"fc-1"`)) || !bytes.Contains(withTool, []byte(`"type":"function_call_output"`)) {
		t.Fatalf("incorrect public wire history (%d, %d items)\nfirst: %s\nnext: %s", len(firstWire.Input), len(secondWire.Input), first, withTool)
	}
	for i, item := range firstWire.Input {
		if !bytes.Equal(item, secondWire.Input[i]) {
			t.Fatalf("wire prefix item %d changed: %s / %s", i, item, secondWire.Input[i])
		}
	}
	if !bytes.Contains(withTool, []byte("出力HEAD")) || !bytes.Contains(withTool, []byte("TAIL終端")) ||
		bytes.Contains(withTool, []byte("middle locator fixture")) {
		t.Fatalf("tool excerpts lost their head or tail: %s", withTool)
	}
	matches, err := s.RecordFind(ctx, record.Query{Kind: record.OperationOutput, Text: "middle locator fixture"})
	checkHostRestore(t, err)
	if len(matches) != 1 || !bytes.Contains(withTool, []byte(matches[0].Locator)) {
		t.Fatalf("full tool output locator missing: %+v, %s", matches, withTool)
	}
	full, err := s.RecordShow(ctx, matches[0].Locator, 0)
	checkHostRestore(t, err)
	if string(full.Bytes) != original {
		t.Fatal("tool output locator did not return the full bytes")
	}
	stop(t, s)
	checkHostRestore(t, h.Close())
	log, err := os.ReadFile(filepath.Join(dir, "window.session.jsonl"))
	checkHostRestore(t, err)
	checkpoint, err := os.ReadFile(filepath.Join(dir, "window.restore-checkpoint"))
	checkHostRestore(t, err)
	if bytes.Contains(log, []byte("do-not-store")) {
		t.Fatal("thinking was stored on the three-layer path")
	}
	store, err := localfile.New(dir)
	checkHostRestore(t, err)
	_, items, _, err := store.ReadSnapshot(ctx, "window")
	checkHostRestore(t, err)
	var boundary record.Locator
	var after int64
	for _, item := range items {
		switch v := item.Data.(type) {
		case sessionstore.HostRecord:
			if v.Context != nil && v.Context.Kind == sessionstore.ContextWindowBoundary {
				if boundary != "" || v.Context.Window.CacheRewarmMinutes != 54 || v.Context.Window.BeforeTokens <= v.Context.Window.AfterTokens {
					t.Fatalf("unexpected boundary: %+v", v.Context.Window)
				}
				boundary = record.Item("window", uint64(item.Sequence))
				after = v.Context.Window.AfterTokens
			}
		case session.Turn:
			if v.ID != "old" && v.PreviousTurnID == "old" &&
				(boundary == "" || v.ContextAdmission.WindowBoundary != boundary || v.ContextAdmission.InputTokens != after || v.ContextAdmission.Measurement != "estimated") {
				t.Fatalf("first request lost boundary or admission: %+v", v.ContextAdmission)
			}
		case sessionstore.ModelResponse:
			if v.Response.ID == "first" && (v.WindowCache.Boundary != boundary || !v.WindowCache.ReadKnown ||
				v.WindowCache.Read != 0 || !v.WindowCache.WriteKnown || v.WindowCache.Write != 7) {
				t.Fatalf("incorrect first-request cache usage: %+v", v.WindowCache)
			}
			if v.Response.ID == "finish" && v.WindowCache.Boundary != "" {
				t.Fatalf("more than one response measured for the boundary: %+v", v.WindowCache)
			}
		}
	}
	var fromFull []byte
	for _, checkpointed := range []bool{false, true} {
		reopen := t.TempDir()
		checkHostRestore(t, os.WriteFile(filepath.Join(reopen, "window.session.jsonl"), log, 0o600))
		if checkpointed {
			checkHostRestore(t, os.WriteFile(filepath.Join(reopen, "window.restore-checkpoint"), checkpoint, 0o600))
		}
		h, requests := open(reopen, finishResponse)
		s, err := h.Resume(ctx, Options{ID: "window", Policy: permission.Unrestricted(), Initial: []inbox.Input{input("after-restart", "Confirm the next action.")}})
		checkHostRestore(t, err)
		replayed := readRequest(s, requests, "replay")
		var wire struct {
			Input []jsontext.Value `json:"input"`
		}
		checkHostRestore(t, json.Unmarshal(replayed, &wire))
		for i, item := range secondWire.Input {
			if !bytes.Equal(item, wire.Input[i]) {
				t.Fatalf("restored request changed item %d in the window", i)
			}
		}
		if !checkpointed {
			fromFull = replayed
		} else if !bytes.Equal(fromFull, replayed) {
			t.Fatal("checkpoint and full replay produced different request bytes")
		}
		stop(t, s)
		checkHostRestore(t, h.Close())
	}
}
