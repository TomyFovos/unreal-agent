package host

import (
	"bytes"
	"context"
	"fmt"
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
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openai"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

func TestOutputLimitResponseHistorySurvivesRestore(t *testing.T) {
	for _, provider := range []string{"openai", "openai-codex", "opencode-go"} {
		t.Run(provider, func(t *testing.T) {
			if provider == "opencode-go" && !buildfeatures.OpenCodeGoEnabled {
				t.Skip("OpenCode Go is disabled in this build")
			}
			testOutputLimitResponseHistory(t, provider)
		})
	}
}

func testOutputLimitResponseHistory(t *testing.T, provider string) {
	t.Helper()
	const id session.ID = "output-limit"
	const successfulUsage = 150
	const goodReasoning = `{"type":"reasoning","id":"reasoning-good","summary":[],"encrypted_content":"successful-cipher"}`
	const badReasoning = `{"type":"reasoning","id":"reasoning-limited","summary":[],"encrypted_content":"discarded-cipher"}`
	const badText = `{"type":"message","id":"message-limited","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"discarded-text","annotations":[]}]}`
	const badCall = `{"type":"function_call","id":"function-limited","call_id":"incomplete-call","name":"Read","arguments":"{\"path\":","status":"incomplete"}`
	const next = "ask for a fresh answer"
	const limited = "try the capped request"
	bodies := make(chan []byte, 4)
	estimates := make(chan int64, 4)
	httpClient := &http.Client{Transport: restoreRoundTripper(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		bodies <- body
		var stream string
		switch {
		case bytes.Contains(body, []byte(next)):
			stream = "data: " + `{"type":"response.completed","response":{"id":"next","status":"completed","output":[]}}` + "\n\n"
		case bytes.Contains(body, []byte(limited)):
			items := strings.Join([]string{badReasoning, badText, badCall}, ",")
			if provider == "openai-codex" {
				for i, item := range []string{badReasoning, badText, badCall} {
					stream += fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", i, item)
				}
				items = ""
			}
			stream += fmt.Sprintf("data: "+`{"type":"response.incomplete","response":{"id":"limited","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[%s],"usage":{"input_tokens":9000,"output_tokens":300}}}`+"\n\n", items)
		default:
			stream = fmt.Sprintf("data: "+`{"type":"response.completed","response":{"id":"initial","status":"completed","output":[%s],"usage":{"input_tokens":%d,"output_tokens":10}}}`+"\n\n", goodReasoning, successfulUsage)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	newHost := func(t *testing.T, dir string) *Host {
		t.Helper()
		type client interface {
			llm.Adapter
			Close() error
		}
		h, err := New(t.Context(), Config{Directory: dir, PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, _ session.ID) (Runtime, error) {
			var adapter client
			var err error
			switch provider {
			case "openai":
				adapter, err = openai.NewClient(openai.Config{HTTPClient: httpClient, BaseURL: "https://openai.test/v1", APIKey: "local-test"})
			case "openai-codex":
				adapter, err = openaicodex.NewClient(openaicodex.Config{HTTPClient: httpClient, AccessToken: "local-test", AccountID: "local-test"})
			case "opencode-go":
				adapter, err = opencodego.NewClient(opencodego.Config{HTTPClient: httpClient, APIKey: "local-test"})
			}
			if err != nil {
				return Runtime{}, err
			}
			b := contextbuilder.NewBuilder()
			b.SetModel(llm.Model{ID: "fixture", ReasoningEffort: llm.ReasoningEffortMax})
			b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider(provider)
			engine, err := contextbuilder.EnableCompaction(b, contextengine.Config{InputBudget: 2400}, contextengine.Constraints{})
			if err != nil {
				adapter.Close()
				return Runtime{}, err
			}
			model := modelFunc(func(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
				estimates <- (contextengine.TokenEstimator{Provider: provider}).Request(request)
				return adapter.Respond(ctx, request, options)
			})
			return Runtime{Builder: b, ContextEngine: engine, LLM: model, Close: adapter.Close, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
		}})
		checkHostRestore(t, err)
		t.Cleanup(func() { h.Close() })
		return h
	}
	turnAdmission := func(t *testing.T, dir string) session.ContextAdmission {
		t.Helper()
		store, err := localfile.New(dir)
		checkHostRestore(t, err)
		_, items, _, err := store.ReadSnapshot(t.Context(), id)
		checkHostRestore(t, err)
		for i := len(items) - 1; i >= 0; i-- {
			if turn, ok := items[i].Data.(session.Turn); ok {
				if turn.ContextAdmission == nil {
					t.Fatal("request was recorded without admission")
				}
				return *turn.ContextAdmission
			}
		}
		t.Fatal("request was not recorded")
		return session.ContextAdmission{}
	}
	dir := t.TempDir()
	h := newHost(t, dir)
	s, err := h.Create(t.Context(), Options{ID: id, PermissionMode: permission.FullAccess})
	checkHostRestore(t, err)
	sub, err := s.Subscribe(0, 32, 64)
	checkHostRestore(t, err)
	defer sub.Cancel()
	request := func(inputID, text, responseID string) ([]byte, int64) {
		t.Helper()
		ctx := timeout(t)
		_, err := s.Submit(ctx, s.Generation, input(inputID, text))
		checkHostRestore(t, err)
		for {
			select {
			case event, ok := <-sub.Events:
				if !ok {
					t.Fatalf("session closed before response %s: %v", responseID, s.Wait(ctx))
				}
				if event.Item != nil {
					if response, ok := event.Item.Data.(sessionstore.ModelResponse); ok && response.Response.ID == responseID {
						return <-bodies, <-estimates
					}
				}
			case <-ctx.Done():
				t.Fatalf("missing response %s", responseID)
			}
		}
	}
	_, initialEstimate := request("initial", "start the task", "initial")
	request("capped", limited, "limited")
	logName := string(id) + ".session.jsonl"
	canonical, err := os.ReadFile(filepath.Join(dir, logName))
	checkHostRestore(t, err)
	store, err := localfile.New(dir)
	checkHostRestore(t, err)
	_, history, ops, err := store.ReadSnapshot(t.Context(), id)
	checkHostRestore(t, err)
	var response llm.Response
	for _, item := range history {
		if record, ok := item.Data.(sessionstore.ModelResponse); ok && record.Response.ID == "limited" {
			response = record.Response
		}
	}
	if response.Stop != llm.StopMaxOutputTokens || response.Usage.OutputTokens != 300 || len(response.Output) != 3 || len(ops) != 0 {
		t.Fatalf("capped response was lost or executed a tool: %+v, operations=%v", response, ops)
	}
	if raw := response.Output[0].Data.(llm.Reasoning).Raw; !bytes.Equal(raw, []byte(badReasoning)) {
		t.Fatalf("capped reasoning did not round-trip: %s", raw)
	}
	if response.Output[1].Data.(llm.Message).Text != "discarded-text" || response.Output[2].Data.(llm.ToolCall) != (llm.ToolCall{CallID: "incomplete-call", Name: "Read", Arguments: `{"path":`, Incomplete: true}) {
		t.Fatal("capped text or tool arguments were lost")
	}
	live, projectedEstimate := request("next", next, "next")
	stop(t, s)
	for _, forbidden := range []string{"discarded-text", "discarded-cipher", "incomplete-call", "max_output_tokens", "Incomplete response returned"} {
		if bytes.Contains(live, []byte(forbidden)) {
			t.Fatalf("next request included %q: %s", forbidden, live)
		}
	}
	for _, required := range []string{next, limited, goodReasoning, `"effort":"max"`} {
		if !bytes.Contains(live, []byte(required)) {
			t.Fatalf("next request omitted %q: %s", required, live)
		}
	}
	admission := turnAdmission(t, dir)
	delta := projectedEstimate - initialEstimate
	if admission.EstimatedInputTokens != projectedEstimate || admission.InputTokens != successfulUsage+delta ||
		admission.MeasuredPrefixTokens != successfulUsage || admission.EstimatedDeltaTokens != delta || len(bodies) != 0 {
		t.Fatalf("unexpected admission or extra request: %+v (estimate=%d delta=%d extra=%d)", admission, projectedEstimate, delta, len(bodies))
	}
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint=%v", checkpoint), func(t *testing.T) {
			dir := t.TempDir()
			checkHostRestore(t, os.WriteFile(filepath.Join(dir, logName), canonical, 0o600))
			if checkpoint {
				store, err := localfile.New(dir)
				checkHostRestore(t, err)
				lease, err := store.AcquireWriter(id)
				checkHostRestore(t, err)
				view, err := store.OpenCheckpointedRestoreView(t.Context(), id)
				checkHostRestore(t, err)
				_, _, err = view.Take(t.Context(), id)
				checkHostRestore(t, err)
				checkHostRestore(t, store.SaveRestoreCheckpoint(id))
				checkHostRestore(t, view.Close())
				checkHostRestore(t, lease.Close())
				_, err = os.Stat(filepath.Join(dir, string(id)+".restore-checkpoint"))
				checkHostRestore(t, err)
			}
			h := newHost(t, dir)
			s, err := h.Resume(t.Context(), Options{ID: id, PermissionMode: permission.FullAccess, Initial: []inbox.Input{input("next", next)}})
			checkHostRestore(t, err)
			stop(t, s)
			if len(bodies) != 1 || len(estimates) != 1 {
				t.Fatalf("restore sent %d requests", len(bodies))
			}
			if wire, estimate := <-bodies, <-estimates; !bytes.Equal(wire, live) || estimate != projectedEstimate {
				t.Fatalf("restore changed the projection: %s\nlive: %s", wire, live)
			}
			if restored := turnAdmission(t, dir); restored != admission {
				t.Fatalf("restore changed admission: %+v, live: %+v", restored, admission)
			}
		})
	}
}
