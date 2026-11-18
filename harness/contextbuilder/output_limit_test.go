package contextbuilder

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestOutputLimitKeepsCompletedToolReceiptWithoutReplayingCall(t *testing.T) {
	for _, engine := range []bool{false, true} {
		t.Run(fmt.Sprintf("engine=%v", engine), func(t *testing.T) {
			b := NewBuilder().(*builder)
			if engine {
				if _, err := EnableCompaction(b, contextengine.Config{}, contextengine.Constraints{}); err != nil {
					t.Fatal(err)
				}
			}
			b.SetRuntimeProvider("openai-codex")
			b.AddModelResponse(llm.Response{Stop: llm.StopMaxOutputTokens, Output: []llm.Item{
				epochCall("read", "Read", `{"path":"failed-call-argument"}`),
			}})
			b.AddToolResult("read", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "committed tool receipt"}}, false)
			result, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			text := requestText(result)
			if strings.Contains(text, "failed-call-argument") || strings.Count(text, "committed tool receipt") != 1 {
				t.Fatalf("incorrect completed tool projection: %s", text)
			}
			for _, item := range result.Request.Input {
				if item.Type == llm.ItemToolCall || item.Type == llm.ItemToolResult {
					t.Fatalf("orphaned native call or receipt in request: %+v", item)
				}
			}
		})
	}
}

func TestOutputLimitInvalidatesMeasurementsFromOldProjection(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			f := newContextFixture(t, "openai-codex", contextengine.Config{InputBudget: 6000})
			f.user(t, "task", "question")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "capped", Type: session.TurnRegular})
			before, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "capped", Response: llm.Response{
				Stop:   llm.StopMaxOutputTokens,
				Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "old capped output"}}},
			}})
			after, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			fingerprint := after.Report.Context.HeaderFingerprint
			if legacy {
				fingerprint = before.Report.Context.HeaderFingerprint
			}
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "follow-up", Type: session.TurnRegular,
				ContextAdmission: &session.ContextAdmission{HeaderFingerprint: fingerprint}})
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "follow-up", Response: llm.Response{Usage: llm.Usage{InputTokens: 1200}}})
			result, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			d := result.Report.Context
			if legacy {
				if d.Measurement != "estimated" || d.InputTokens != d.EstimatedInputTokens {
					t.Fatalf("used measurement for the old request body: %+v", d)
				}
			} else if d.MeasuredInputTokens != 1200 || d.InputTokens != 1200 {
				t.Fatalf("lost matching measurement: %+v", d)
			}
			// Reserving the old unit position keeps an existing compacted boundary
			// before the following user's message, even after the output is filtered.
			f.user(t, "latest", "new user input")
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: "summary", Through: "unit-00000000000000000002", Text: "old summary"})
			result, err = f.b.Build()
			if err != nil || !strings.Contains(requestText(result), "new user input") || strings.Contains(requestText(result), "old capped output") {
				t.Fatalf("old summary boundary changed: %v", err)
			}
		})
	}
}

func TestOutputLimitDoesNotInstallFailedCompaction(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{FallbackWindow: 10000, InputBudget: 8000})
	f.turn(t, "first", "preserve this request", strings.Repeat("Earlier content. ", 900))
	f.user(t, "next", "follow up")
	const partial = "partial summary"
	adapter := summaryAdapter(func(llm.Request) llm.Response {
		return llm.Response{Stop: llm.StopMaxOutputTokens, Output: []llm.Item{
			{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: partial}},
		}}
	})
	var record sessionstore.ContextRecord
	err := f.b.PrepareContext(t.Context(), adapter, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error {
		if r.Kind == "summary_failed" {
			record = r
		}
		f.b.ApplyContext(r)
		return nil
	}, true)
	var failure *SummaryModelError
	if !errors.As(err, &failure) || record.Text != string(llm.StopMaxOutputTokens) || len(record.Items) != 1 ||
		record.Items[0].Data.(llm.Message).Text != partial {
		t.Fatalf("failed compaction lost its record: %+v %v", record, err)
	}
	result, err := f.b.Build()
	if err != nil || strings.Contains(requestText(result), partial) || !strings.Contains(requestText(result), "preserve this request") {
		t.Fatalf("failed compaction altered the request: %v", err)
	}
}
