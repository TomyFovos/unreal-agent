package contextbuilder

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/claudeprompt"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

var admissionProviders = []string{"claude-code", "claude", "openai-codex", "openai", "opencode-go", "openrouter", "fireworks", "ollama"}

func TestAdmissionIgnoresLargeJSONOverhead(t *testing.T) {
	for _, provider := range admissionProviders {
		t.Run(provider, func(t *testing.T) {
			f := newContextFixture(t, provider, contextengine.Config{InputBudget: 6000})
			f.user(t, "task", "Continue")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Output: []llm.Item{
				{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: []byte(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("opaque", 100000) + `"}`)}},
				{Type: llm.ItemMessage, ProviderID: strings.Repeat("id", 100000), Data: llm.Message{Role: llm.RoleAssistant, Text: "ok"}},
			}}})
			if appendOnlyProviders[provider] {
				adapter := summaryAdapter(func(llm.Request) llm.Response {
					t.Fatal("JSON overhead triggered unnecessary compaction")
					return llm.Response{}
				})
				if err := f.b.PrepareContext(t.Context(), adapter, llm.RequestOptions{}, func(r sessionstore.ContextRecord) error { f.b.ApplyContext(r); return nil }, false); err != nil {
					t.Fatal(err)
				}
			}
			built, err := f.b.Build()
			if err != nil {
				t.Fatal("JSON envelope blocked content that fits", err)
			}
			d := built.Report.Context
			if d.InputTokens > d.Budget.Input || d.Measurement != "estimated" || d.Estimator == "" {
				t.Fatal("incorrect token diagnostic", d)
			}
			if provider != "claude-code" && provider != "claude" && d.RequestBytes <= d.Budget.Input {
				t.Fatal("fixture lacks JSON overhead", d.RequestBytes)
			}
		})
	}
}

func TestAdmissionMeasuredPrefixPlusDeltaForEveryProvider(t *testing.T) {
	for _, provider := range admissionProviders {
		t.Run(provider, func(t *testing.T) {
			f := newContextFixture(t, provider, contextengine.Config{InputBudget: 6000})
			f.user(t, "task", strings.Repeat("prefix", 10000))
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
			prior := f.b.turnEstimates["first"].tokens
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 1234}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "answer!!"}}}}})
			f.user(t, "next", "next")
			built, err := f.b.Build()
			if err != nil {
				t.Fatal(err)
			}
			d := built.Report.Context
			delta := int64(3)
			if f.b.tokenEstimator().Claude() {
				delta = f.b.tokenEstimator().Text(claudeprompt.Plan(built.Request.Input).Full) - prior
			}
			if d.EstimatedInputTokens <= d.Budget.Input || d.MeasuredInputTokens != 1234 || d.EstimatedDeltaTokens != delta || d.InputTokens != 1234+delta || d.EstimatedInputTokens != prior+delta || d.Measurement != "measured-prefix+estimated-delta" {
				t.Fatalf("measured prefix/delta not respected: %+v", d)
			}
			// The same canonical history must restore the same anchor and delta.
			replay := newContextFixture(t, provider, f.b.contextConfig)
			for _, item := range f.history {
				replay.item(t, item.Kind, item.Data)
			}
			restored, err := replay.b.Build()
			if err != nil || restored.Report.Context.InputTokens != d.InputTokens || restored.Report.Context.Measurement != d.Measurement {
				t.Fatal("resume lost measured prefix", err)
			}
			f.user(t, "large-tail", strings.Repeat("x", 24000))
			_, err = f.b.Build()
			if provider != "claude-code" && provider != "claude" && !contextengine.IsBudgetError(err) {
				t.Fatal("measured prefix hid over-window tail", err)
			}
		})
	}
}

func TestAdmissionInvalidatesMeasuredPrefixAfterProjectionOrHeaderChange(t *testing.T) {
	for _, change := range []string{"model", "provider", "instructions", "tools", "revision", "projection", "summary", "missing-usage"} {
		t.Run(change, func(t *testing.T) {
			f := newContextFixture(t, "openai", contextengine.Config{})
			f.user(t, "task", "question")
			f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
			f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 999999}}})
			switch change {
			case "model":
				f.b.SetModel(llm.Model{ID: "other"})
			case "provider":
				f.b.SetRuntimeProvider("openrouter")
			case "revision":
				f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: "openai", Model: "synthetic-model", Revision: 7}, true)
			case "instructions":
				f.b.SetPermissionPrompt("changed permission")
			case "tools":
				f.b.AddTool(llm.Tool{Type: llm.ToolFunction, Name: "new"})
			case "projection":
				f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: f.b.lastUnit, Items: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "replacement"}}}})
			case "summary":
				f.b.ApplyContext(sessionstore.ContextRecord{Kind: "summary", Through: f.b.lastUnit, Text: "summary"})
			case "missing-usage":
				f.item(t, sessionstore.ItemTurn, session.Turn{ID: "later", Type: session.TurnRegular})
				response := llm.Response{}
				f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "later", Response: response})
			}
			built, err := f.b.Build()
			if err != nil {
				t.Fatal("stale measured input blocked new prefix", err)
			}
			if d := built.Report.Context; d.Measurement != "estimated" || d.MeasuredInputTokens != 0 || d.InputTokens != d.EstimatedInputTokens {
				t.Fatal("stale prefix matched", d)
			}
		})
	}
}

func TestAdmissionUsesLastClaudeCallInputInsteadOfSummedSteps(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{})
	f.user(t, "task", "question")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 50000}, Generation: &llm.GenerationStatistics{ContextTokens: 2000, ContextKnown: true}}})
	built, err := f.b.Build()
	if err != nil || built.Report.Context.InputTokens != 2000 || built.Report.Context.MeasuredInputTokens != 2000 {
		t.Fatal("summed Claude steps used as context input", err)
	}
}

func TestAdmissionPilot2WithoutRawUsesTokens(t *testing.T) {
	f := newContextFixture(t, "openai", contextengine.Config{FallbackWindow: 786432, InputBudget: 774599})
	f.user(t, "task", "Inspect the output")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Output: []llm.Item{epochCall("read", "Bash", `{"command":"read output"}`)}}})
	epochReceipt(t, f, "read", "", false)
	request := f.b.request
	request.Input = f.b.deriveItems()
	const bytes = 1024449
	overhead := contextengine.JSONBytes(request)
	result := llm.ToolResult{CallID: "read", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("x", bytes-int(overhead))}}}
	f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: f.b.lastUnit, Items: []llm.Item{{Type: llm.ItemToolResult, Data: result}}})
	built, err := f.b.Build()
	if err != nil {
		t.Fatal("pilot2-like payload still refused", err)
	}
	d := built.Report.Context
	if d.RequestBytes != bytes || d.Budget.Input != 774599 || d.EstimatedInputTokens >= d.Budget.Input || d.Measurement != "estimated" {
		t.Fatalf("wrong units: %+v", d)
	}
	for _, item := range built.Request.Input {
		if r, ok := item.Data.(llm.Reasoning); ok && len(r.Raw) > 0 {
			t.Fatal("fixture unexpectedly has Raw")
		}
	}
	// The new input is still counted when the previous successful input is known.
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "pilot", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "pilot", Response: llm.Response{Usage: llm.Usage{InputTokens: 5700}}})
	f.user(t, "next", "continue")
	built, err = f.b.Build()
	if err != nil || built.Report.Context.InputTokens != 5702 {
		t.Fatal("pilot2 measured prefix not used", err)
	}
	t.Logf("payload bytes=%d budget tokens=%d estimated input tokens=%d", bytes, d.Budget.Input, d.EstimatedInputTokens)
}

func TestAdmissionGenuinelyOverWindowStillRefuses(t *testing.T) {
	for _, provider := range []string{"openai-codex", "openai", "opencode-go", "openrouter", "fireworks", "ollama"} {
		for _, measured := range []bool{false, true} {
			name := provider + "/estimated"
			if measured {
				name = provider + "/measured"
			}
			t.Run(name, func(t *testing.T) {
				f := newContextFixture(t, provider, contextengine.Config{})
				f.b.SetContextRuntime(sessionstore.RuntimeSelection{Provider: provider, Model: "synthetic-model", ContextWindow: 20000}, true)
				f.user(t, "task", strings.Repeat("x", 80004))
				if measured {
					f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
					f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 20001}}})
				}
				built, err := f.b.Build()
				if !contextengine.IsBudgetError(err) || built.Report.Context.InputTokens <= built.Report.Context.Budget.Window {
					t.Fatal("genuinely oversized input admitted", err)
				}
			})
		}
	}
}

func TestAdmissionRecordsRemainLabelledAfterJSONRoundTrip(t *testing.T) {
	f := newContextFixture(t, "openai", contextengine.Config{})
	f.user(t, "task", "question")
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(built.Report.Context)
	if err != nil || !strings.Contains(string(data), `"Measurement":"estimated"`) || !strings.Contains(string(data), `"Estimator":"responsesapi-content:openai"`) {
		t.Fatal("estimate became an unlabelled count", err)
	}
}

func TestAdmissionRetainsLastSuccessfulPrefixAcrossFailedRequest(t *testing.T) {
	f := newContextFixture(t, "openai", contextengine.Config{InputBudget: 6000})
	f.user(t, "task", "question")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 1234}}})
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "failed", Type: session.TurnRegular})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "failed", Response: llm.Response{Usage: llm.Usage{InputTokens: 999999}, Failure: &llm.Failure{Code: "failed", Message: "failed!!"}}})
	built, err := f.b.Build()
	if err != nil || built.Report.Context.MeasuredInputTokens != 1234 || built.Report.Context.InputTokens != 1236 {
		t.Fatal("failure replaced the last successful prefix", err)
	}
}

func TestAdmissionReportedZeroAndIntegerOverflow(t *testing.T) {
	for _, input := range []int64{0, 9223372036854775807} {
		f := newContextFixture(t, "openai", contextengine.Config{InputBudget: 6000})
		f.user(t, "task", "question")
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: input, Raw: []byte(`{"input_tokens":0}`)}, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "tail"}}}}})
		built, err := f.b.Build()
		if input == 0 {
			if err != nil || built.Report.Context.InputTokens != 1 || built.Report.Context.Measurement != "measured-prefix+estimated-delta" {
				t.Fatal("explicit measured zero was discarded", err)
			}
		} else if !contextengine.IsBudgetError(err) || built.Report.Context.InputTokens != input {
			t.Fatal("large measured input wrapped around", err)
		}
	}
}

func TestAdmissionDoesNotReuseOtherHistoricalRuntimeInput(t *testing.T) {
	for _, runtime := range []struct{ provider, model string }{{"openrouter", "synthetic-model"}, {"openai", "other-model"}} {
		f := newContextFixture(t, "openai", contextengine.Config{})
		f.b.SetHistoryRuntime(runtime.provider, runtime.model)
		f.user(t, "task", "question")
		f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular})
		f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 999999}}})
		built, err := f.b.Build()
		if err != nil || built.Report.Context.Measurement != "estimated" {
			t.Fatal("other runtime input contaminated admission", err)
		}
	}
}

func TestAdmissionToolSchemasCountOnce(t *testing.T) {
	f := newContextFixture(t, "openai", contextengine.Config{})
	f.b.AddTool(llm.Tool{Type: llm.ToolFunction, Name: "test", Description: strings.Repeat("schema", 2000)})
	f.user(t, "task", "question")
	used := f.b.estimateRequest()
	f.b.contextConfig.InputBudget = used
	built, err := f.b.Build()
	if err != nil || built.Report.Context.Budget.SchemaReserve != 0 || built.Report.Context.InputTokens != used {
		t.Fatal("schemas were reserved as well as counted", err)
	}
	f.b.contextConfig.InputBudget = used - 1
	if _, err := f.b.Build(); !contextengine.IsBudgetError(err) {
		t.Fatal("inclusive token-budget boundary changed", err)
	}
}

func TestAdmissionReplayChecksRecordedHeader(t *testing.T) {
	f := newContextFixture(t, "openai", contextengine.Config{})
	f.user(t, "task", "question")
	f.item(t, sessionstore.ItemTurn, session.Turn{ID: "first", Type: session.TurnRegular, ContextAdmission: &session.ContextAdmission{HeaderFingerprint: strings.Repeat("0", 64)}})
	f.item(t, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "first", Response: llm.Response{Usage: llm.Usage{InputTokens: 999999}}})
	built, err := f.b.Build()
	if err != nil || built.Report.Context.Measurement != "estimated" {
		t.Fatal("changed replay header reused old measurement", err)
	}
}
