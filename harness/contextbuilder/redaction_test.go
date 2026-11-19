package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestToolContentRedactionPreservesCodeAndCanonicalReplay(t *testing.T) {
	const output = "state_store.py\ndef read(token: str):\n    secret = load()\n    return production_writeback()\n" +
		"sk-synthetic-private\nBearer synthetic-private\neyJsynthetic.payload.signature\n" +
		"private_key=-----BEGIN PRIVATE KEY-----\nsynthetic-private\n-----END PRIVATE KEY-----\nrevision: keep-revision\n"
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			newFixture := func() *contextFixture {
				if compact {
					return newContextFixture(t, "openai-codex", contextengine.Config{})
				}
				b := NewBuilder().(*builder)
				b.SetRuntimeProvider("openai-codex")
				b.ConfigureRuntime(llm.Model{ID: "synthetic-model"}, "required runtime instructions", nil, nil, false, true)
				return &contextFixture{b: b}
			}
			f := newFixture()
			f.user(t, "task", "Read state_store.py")
			fixtureExchange(t, f, "read", "read", `{"path":"state_store.py"}`, output)
			canonical, err := json.Marshal(f.history)
			if err != nil {
				t.Fatal(err)
			}
			built, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			text := requestText(built)
			for _, keep := range []string{"state_store.py", "def read(", "return production_writeback()", "keep-revision", "[REDACTED]"} {
				if !strings.Contains(text, keep) {
					t.Fatalf("lost useful code/result: %s", keep)
				}
			}
			for _, value := range []any{built.Request, built.Package, built.Report, ContextDebug(built)} {
				encoded, err := json.Marshal(value)
				if err != nil || strings.Contains(string(encoded), "synthetic-private") || strings.Contains(string(encoded), "eyJsynthetic") {
					t.Fatal("credential leaked into model context or diagnostics", err)
				}
			}
			after, _ := json.Marshal(f.history)
			if string(after) != string(canonical) || !strings.Contains(string(after), "synthetic-private") {
				t.Fatal("canonical history was rewritten")
			}
			replay := newFixture()
			for _, item := range f.history {
				appendFixtureRecord(t, replay, item.Kind, item.Data)
			}
			resumed, err := replay.b.Build()
			if err != nil || !reflect.DeepEqual(built.Request, resumed.Request) {
				t.Fatal("old canonical records changed on replay", err)
			}
		})
	}
}

func TestRedactedReceiptRetainsTranslatedTerminalState(t *testing.T) {
	for _, state := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(state), func(t *testing.T) {
			status, result := translatedSecretReceipt(t, state, "fixture.txt")
			f := newContextFixture(t, "openai-codex", contextengine.Config{})
			f.user(t, "task", "Inspect fixture.txt")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: status.TurnID, Type: session.TurnRegular})
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: status.TurnID, Response: llm.Response{Output: []llm.Item{fixtureCall(status.CallID, "read", `{"path":"fixture.txt"}`)}}})
			item := sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: status}
			f.b.SetHistoryItem(item)
			f.b.AddTypedToolResult(result, false)
			built, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range built.Request.Input {
				if r, ok := item.Data.(llm.ToolResult); ok && r.CallID == status.CallID {
					if r.State != result.State || !strings.Contains(r.Output[0].Value, "[REDACTED]") || strings.Contains(r.Output[0].Value, "SYNTHETIC_SECRET_BODY") || !strings.Contains(r.Output[0].Value, "content") {
						t.Fatal("receipt body or translated state was lost", r)
					}
					found = true
				}
			}
			if !found {
				t.Fatal("partially redacted native receipt missing")
			}
		})
	}
}

func TestLegacyBuilderRedactedCallDropsNativeReplayState(t *testing.T) {
	b := NewBuilder()
	b.AddModelResponse(llm.Response{Output: []llm.Item{
		{Type: llm.ItemReasoning, ProviderID: "private-reasoning-id", Data: llm.Reasoning{Raw: []byte(`{"type":"reasoning","encrypted_content":"private-reasoning-body"}`)}},
		fixtureCall("read", "read", `{"path":"state_store.py","token":"synthetic-private"}`),
	}})
	b.AddToolResult("read", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "keep result"}}, false)
	built, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range built.Request.Input {
		if item.Type != llm.ItemMessage || item.ProviderID != "" {
			t.Fatal("redacted call retained native replay state")
		}
	}
	text := requestText(built)
	if strings.Contains(text, "synthetic-private") || strings.Contains(text, "private-reasoning") || !strings.Contains(text, "state_store.py") || !strings.Contains(text, "keep result") || !strings.Contains(text, "[REDACTED]") {
		t.Fatal("legacy builder discarded useful content or leaked credentials")
	}
}

func TestHistoricalToolProjectionsKeepOriginalAndRedactionMetadata(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.user(t, "task", "Read source.py")
	fixtureExchange(t, f, "read", "read", `{"path":"source.py"}`, "keep result")
	var unit contextengine.Unit
	for _, u := range f.b.engine.Units() {
		if u.Kind == contextengine.ToolResult {
			unit = u
		}
	}
	r := unit.Item.Data.(llm.ToolResult)
	r.Output[0].Value = "source.py token=synthetic-private ; keep result"
	for _, kind := range []string{"projection", "prune"} {
		f.b.ApplyContext(sessionstore.ContextRecord{Kind: kind, UnitID: unit.ID, Items: []llm.Item{{Type: llm.ItemToolResult, Data: r}}})
		built, err := f.b.Build()
		if err != nil || strings.Contains(requestText(built), "synthetic-private") || !strings.Contains(requestText(built), "keep result") {
			t.Fatal("legacy projection lost public content or leaked credentials", err)
		}
	}
	var redactions []llm.ActivityRecord
	if err := f.b.RecordRedactions(func(r llm.ActivityRecord) error {
		redactions = append(redactions, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(redactions) != 2 || redactions[0].Redaction.Reason != contextengine.PartialRedaction || redactions[1].Redaction.Reason != contextengine.PartialRedaction {
		t.Fatal("historical redaction observations were lost", redactions)
	}
}

func TestRedactedCallProjectionMakesItsGroupPortable(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.user(t, "task", "Read source.py")
	fixtureExchange(t, f, "read", "read", `{"path":"source.py"}`, "keep result")
	if _, err := f.b.Build(); err != nil {
		t.Fatal(err)
	}
	for _, u := range f.b.engine.Units() {
		if u.Kind != contextengine.ToolRequest {
			continue
		}
		call := u.Item.Data.(llm.ToolCall)
		call.Arguments = `{"path":"source.py","token":"synthetic-private"}`
		f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: u.ID, Items: []llm.Item{{Type: llm.ItemToolCall, ProviderID: "fc-read", Data: call}}})
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range built.Request.Input {
		if item.Type != llm.ItemMessage || item.ProviderID != "" {
			t.Fatal("redacted projection retained cached native group")
		}
	}
	text := requestText(built)
	if strings.Contains(text, "synthetic-private") || !strings.Contains(text, "source.py") || !strings.Contains(text, "keep result") {
		t.Fatal("redacted projection lost useful content or leaked credentials")
	}
}
