package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

func TestResponsesContextToolsRoundTripInReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	testsession.WriteSettledChildren(t, dir, 2, 128)
	const excerpt = "Use the verbatim evidence"
	original := excerpt + "\r\n" + strings.Repeat("証拠", 7000)
	source := make(chan record.Locator, 1)
	var loc record.Locator
	var once sync.Once
	var calls atomic.Int32
	actions := []contextstate.Action{contextstate.Decide, contextstate.SetPlan, contextstate.AddRule, contextstate.AskUser,
		contextstate.AnswerUser, contextstate.Accept, contextstate.Reject, contextstate.Show, contextstate.ShowRecord, contextstate.FindRecord}
	allResults := make(chan map[string]string, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output []struct {
					Text string `json:"text"`
				} `json:"output"`
			} `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		once.Do(func() {
			select {
			case loc = <-source:
			case <-ctx.Done():
			}
			have := map[string]bool{}
			for _, tool := range req.Tools {
				have[tool.Name] = true
			}
			for _, name := range actions {
				if !have[string(name)] {
					t.Errorf("native tool %s is absent", name)
				}
			}
		})
		i := int(calls.Add(1) - 1)
		output := []any{}
		if i < len(actions) {
			plan := "Verify the evidence"
			args := []any{
				contextstate.DecisionNote{ID: "decision", Decision: "Retain the evidence", Basis: []record.Locator{loc}},
				contextstate.PlanNote{ID: "verify", Text: &plan, Status: contextstate.PlanDoing, Decisions: []string{"decision"}},
				contextstate.RuleNote{ID: "rule", Excerpt: excerpt, Source: loc},
				contextstate.QuestionNote{ID: "question", Text: "Which evidence?"},
				contextstate.AnswerNote{ID: "question", Source: loc},
				contextstate.ReviewNote{Handle: "spawn-0000"},
				contextstate.ReviewNote{Handle: "spawn-0001"},
				struct{}{},
				contextstate.RecordSelection{Locator: loc},
				contextstate.RecordSearch{Kind: record.UserMessage, Text: excerpt},
			}
			arg, err := json.Marshal(args[i])
			if err != nil {
				t.Error(err)
			}
			name := string(actions[i])
			output = append(output, map[string]string{"type": "function_call", "id": name, "call_id": name, "name": name, "arguments": string(arg)})
		} else if i == len(actions) {
			results := make(map[string]string)
			for _, item := range req.Input {
				if item.Type == "function_call_output" && len(item.Output) == 1 {
					results[item.CallID] = item.Output[0].Text
				}
			}
			allResults <- results
		}
		res, err := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("response", i), "status": "completed", "output": output}})
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", res)
	}))
	defer endpoint.Close()
	model := provider.Model{ID: "gpt-fixture"}
	registry, err := provider.New(provider.Defaults(map[string][]provider.Model{"openai-codex": {model}})...)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	factory, _, err := NewRuntimeFactory(RuntimeConfig{
		Identity: RuntimeIdentity{Version: 1, Workspace: workspace, Profile: profile.Default(), ReasoningEffort: llm.ReasoningEffortLow,
			Provider: provider.Selection{Version: 1, Provider: "openai-codex", Model: model, Endpoint: endpoint.URL, Auth: credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "fixture"}, MaxAttempts: 1, Source: "test"}},
		SessionDirectory: dir, Providers: registry,
		Credentials: credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
			return credential.Material{Token: credential.NewSecret("fixture-token"), AccountID: "fixture-account"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := permission.New(permission.Config{ReadRoots: []string{workspace}, NetworkOrigins: []string{endpoint.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	h, err := host.New(ctx, host.Config{Directory: dir, Build: factory, PermissionMaximum: policy})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Resume(ctx, host.Options{ID: "main", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Submit(ctx, s.Generation, inbox.Input{ID: "text", Kind: inbox.InputExternal, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	user := record.Input(string(s.ID), uint64(r.Sequence), record.UserMessage)
	source <- user
	var results map[string]string
	select {
	case results = <-allResults:
	case <-ctx.Done():
		view, _ := s.Inspect(0, 128)
		t.Fatalf("Responses tools did not finish, %d calls, %s: %v", calls.Load(), view.Failure, ctx.Err())
	}
	for _, action := range actions[:7] {
		var receipt contextstate.Result
		if err := json.Unmarshal([]byte(results[string(action)]), &receipt); err != nil || receipt.Locator == "" {
			t.Fatalf("%s produced no receipt: %q (%v)", action, results[string(action)], err)
		}
	}
	var page struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(results[string(contextstate.ShowRecord)]), &page); err != nil || page.Text != original {
		t.Fatalf("native RecordShow changed the original (%d bytes): %v", len(page.Text), err)
	}
	var matches []struct {
		Locator record.Locator `json:"locator"`
		Excerpt struct {
			Text string `json:"text"`
		} `json:"excerpt"`
	}
	if err := json.Unmarshal([]byte(results[string(contextstate.FindRecord)]), &matches); err != nil ||
		len(matches) != 1 || matches[0].Locator != user || matches[0].Excerpt.Text != excerpt {
		t.Fatalf("native RecordFind result: %+v %v", matches, err)
	}
	wanted := jsontext.Value(results[string(contextstate.Show)])
	var projection contextstate.State
	if err := json.Unmarshal(wanted, &projection); err != nil {
		t.Fatal(err)
	}
	if projection.Decisions["decision"].Locator == "" || projection.Rules["rule"].Locator != user ||
		len(projection.Plan.Steps) != 1 || projection.QuestionsToUser.Processed.Count != 1 ||
		projection.Children.Processed.Count != 2 || len(projection.Children.Current) != 0 {
		t.Fatalf("native StateShow lost explicit updates: %+v", projection)
	}
	v, err := s.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	reviews := make(map[string]sessionstore.ChildReview)
	for _, item := range v.History.Items {
		if h, ok := item.Data.(sessionstore.HostRecord); ok && h.ChildReview != nil {
			reviews[h.ChildReview.Handle] = *h.ChildReview
		}
	}
	if len(reviews) != 2 || reviews["spawn-0000"].Decision != "accept" || reviews["spawn-0001"].Decision != "reject" ||
		reviews["spawn-0001"].Reason != "" || v.Permissions.Mode != permission.ReadOnly || len(v.Permissions.Pending) != 0 {
		t.Fatalf("native read-only review receipts: %+v, mode: %+v", reviews, v.Permissions)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := stored.ContextState(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	replay.Session.ID = string(s.ID)
	encoded, err := json.Marshal(replay.Render(0))
	if err != nil || !bytes.Equal(encoded, wanted) {
		t.Fatal("native StateShow differs from replayed bytes", err)
	}
}
