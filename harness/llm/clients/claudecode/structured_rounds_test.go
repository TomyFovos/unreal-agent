//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type layerRoundHarness struct {
	request       llm.Request
	schema        *actionSchema
	options       llm.RequestOptions
	limits        ToolBridgeConfig
	inputs        []string
	models        []llm.Model
	calls         []llm.ToolCall
	frames        []string
	generateError error
	receiptError  error
	failed        bool
}

func TestStructuredLayerReceiptContextBudgetFailure(t *testing.T) {
	r := newLayerRounds(t, layerRead, layerFinal)
	r.options.RefreshContext = func(context.Context, int64) (llm.Request, int64, error) {
		if len(r.calls) > 0 {
			return llm.Request{}, 0, &contextengine.Error{Code: "required_context_exceeds_budget"}
		}
		return r.request, 12000, nil
	}
	response, err := r.run(t.Context())
	requireCode(t, err, "bridge_context_budget")
	if len(r.calls) != 1 || len(r.inputs) != 1 || len(response.Output) != 0 {
		t.Fatal("receipt budget failure repeated work or emitted Final")
	}
	if len(r.request.Input) == 0 || r.request.Input[len(r.request.Input)-1].Type != llm.ItemToolResult {
		t.Fatal("completed canonical receipt was lost")
	}
}

func newLayerRounds(t *testing.T, frames ...string) *layerRoundHarness {
	t.Helper()
	r := &layerRoundHarness{frames: frames, limits: (ToolBridgeConfig{MaxActionRounds: 8, TimeoutMillis: 1000, ResultBytes: 4096}).limits()}
	r.request = textRequest()
	r.request.Model = llm.Model{ID: "synthetic-model", ReasoningEffort: llm.ReasoningEffortHigh}
	registry := tool.NewRegistry(tool.StaticTranslators{}, tool.ReadName, tool.GlobName)
	for _, def := range registry.StaticDefinitions() {
		r.request.Tools = append(r.request.Tools, def.Tool)
	}
	var err error
	r.schema, err = newActionSchema(r.request.Tools)
	if err != nil {
		t.Fatal(err)
	}
	r.options = llm.RequestOptions{ActionScope: "canonical-unit-input", InputBudget: 12000}
	r.options.RefreshContext = func(context.Context, int64) (llm.Request, int64, error) { return r.request, 12000, nil }
	r.options.Tools = func(_ context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		if len(response.Output) != 1 || response.Output[0].Type != llm.ItemToolCall {
			t.Fatal("protocol helper became public output")
		}
		call := response.Output[0].Data.(llm.ToolCall)
		r.calls = append(r.calls, call)
		if r.receiptError != nil {
			return nil, r.receiptError
		}
		result := llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "canonical receipt for " + call.Name}}}
		// Fake the Host's refreshed canonical projection. structuredPrompt must
		// deduplicate this receipt against its explicit machine-owned last result.
		r.request.Input = append(r.request.Input, llm.Item{Type: llm.ItemToolResult, Data: result})
		return []llm.ToolOutcome{{Result: result, Failed: r.failed}}, nil
	}
	return r
}

func (r *layerRoundHarness) run(ctx context.Context) (llm.Response, error) {
	return runStructuredRounds(ctx, r.request, r.options, r.schema, r.limits, func(_ context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
		r.inputs = append(r.inputs, input)
		r.models = append(r.models, model)
		if r.generateError != nil {
			return actionProposal{}, llm.Response{}, r.generateError
		}
		if len(r.inputs) > len(r.frames) {
			return actionProposal{}, llm.Response{}, errors.New("fixture generation exhausted")
		}
		p, err := schema.parse(jsontext.Value(r.frames[len(r.inputs)-1]))
		return p, llm.Response{Model: "synthetic-observed-model", Usage: llm.Usage{InputTokens: 2, OutputTokens: 1}}, err
	})
}

const layerRead = `{"type":"action","action":{"id":"read-1","tool":"read","arguments":{"path":"fixture.txt"}}}`
const layerGlob = `{"type":"action","action":{"id":"glob-1","tool":"glob","arguments":{"path":".","glob":"*.txt"}}}`
const layerFinal = `{"type":"final","final":{"message":"public final"}}`

func TestStructuredLayerRoundsReceiptAndFinal(t *testing.T) {
	r := newLayerRounds(t, layerRead, layerGlob, layerFinal)
	response, err := r.run(t.Context())
	if err != nil || len(r.calls) != 2 || len(r.inputs) != 3 || len(response.Output) != 1 || response.Output[0].Data.(llm.Message).Text != "public final" {
		t.Fatal("round lifecycle", err)
	}
	if r.calls[0].Name != "read" || r.calls[1].Name != "glob" || r.calls[0].CallID == r.calls[1].CallID || !strings.HasPrefix(r.calls[0].CallID, "structured-") {
		t.Fatal("tool order/action identity")
	}
	if strings.Contains(r.inputs[0], "canonical receipt") || strings.Count(r.inputs[1], "canonical receipt for read") != 1 || strings.Count(r.inputs[2], "canonical receipt for glob") != 1 {
		t.Fatal("receipt missing or duplicated")
	}
	for _, model := range r.models {
		if model != r.request.Model {
			t.Fatal("generation runtime was not pinned")
		}
	}
	if response.Usage.InputTokens != 2 {
		t.Fatal("canonical action round usage double counted in Final")
	}
	if len(r.request.Input) != len(textRequest().Input)+2 {
		t.Fatal("Host context fixture mutation unexpected")
	}
	if strings.Contains(response.Output[0].Data.(llm.Message).Text, `"type":`) {
		t.Fatal("protocol leaked into public Final")
	}
}

func TestStructuredLayerRoundsReplayCollisionAndBounds(t *testing.T) {
	t.Run("same-payload", func(t *testing.T) {
		r := newLayerRounds(t, layerRead, layerRead, layerFinal)
		response, err := r.run(t.Context())
		if err != nil || len(r.calls) != 1 || len(r.inputs) != 3 || response.Usage.InputTokens != 4 || r.inputs[1] != r.inputs[2] {
			t.Fatal("replayed side effect or receipt/usage lost", err)
		}
	})
	t.Run("semantic-payload", func(t *testing.T) {
		r := newLayerRounds(t, layerRead, `{"action":{"arguments":{"path":"fixture.txt"},"tool":"read","id":"read-1"},"type":"action"}`, layerFinal)
		_, err := r.run(t.Context())
		if err != nil || len(r.calls) != 1 {
			t.Fatal("JSON order caused duplicate side effect", err)
		}
	})
	t.Run("collision", func(t *testing.T) {
		r := newLayerRounds(t, layerRead, strings.Replace(layerRead, "fixture.txt", "different.txt", 1))
		_, err := r.run(t.Context())
		requireCode(t, err, "bridge_duplicate_conflict")
		if len(r.calls) != 1 {
			t.Fatal("conflicting action executed")
		}
	})
	for _, frames := range [][]string{{layerRead, layerGlob}, {layerRead, layerRead}} {
		r := newLayerRounds(t, frames...)
		r.limits.MaxActionRounds = 1
		_, err := r.run(t.Context())
		requireCode(t, err, "action_round_limit")
		if len(r.calls) != 1 || len(r.inputs) != 2 {
			t.Fatal("round limit executed more work")
		}
	}
}

func TestStructuredLayerRoundsFailuresAndCancellation(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		r := newLayerRounds(t, layerRead)
		r.generateError = assistantFailure(jsontext.Value(`"unknown"`))
		response, err := r.run(t.Context())
		requireCode(t, err, "subprocess_failure")
		if len(r.calls) != 0 || len(response.Output) != 0 || len(r.inputs) != 1 {
			t.Fatal("provider failure fell back or executed")
		}
	})
	t.Run("permission-failure-receipt", func(t *testing.T) {
		r := newLayerRounds(t, layerRead, layerFinal)
		r.failed = true
		_, err := r.run(t.Context())
		if err != nil || !strings.Contains(r.inputs[1], `"Failed":true`) {
			t.Fatal("denial/failure receipt lost", err)
		}
	})
	t.Run("callback", func(t *testing.T) {
		r := newLayerRounds(t, layerRead)
		r.receiptError = errors.New("private-sensitive")
		_, err := r.run(t.Context())
		requireCode(t, err, "bridge_host_failure")
		if strings.Contains(err.Error(), "private-sensitive") {
			t.Fatal("raw callback error leaked")
		}
	})
	t.Run("receipt-mismatch", func(t *testing.T) {
		r := newLayerRounds(t, layerRead)
		r.options.Tools = func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
			return []llm.ToolOutcome{{Result: llm.ToolResult{CallID: "wrong"}}}, nil
		}
		_, err := r.run(t.Context())
		requireCode(t, err, "bridge_receipt_invalid")
	})
	t.Run("cancel-before-generation", func(t *testing.T) {
		r := newLayerRounds(t, layerRead)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := r.run(ctx)
		if !errors.Is(err, context.Canceled) || len(r.inputs) != 0 || len(r.calls) != 0 {
			t.Fatal("cancel ignored", err)
		}
	})
	t.Run("cancel-during-tool", func(t *testing.T) {
		r := newLayerRounds(t, layerRead, layerFinal)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.options.Tools = func(ctx context.Context, _ llm.Response) ([]llm.ToolOutcome, error) { cancel(); return nil, ctx.Err() }
		_, err := r.run(ctx)
		if !errors.Is(err, context.Canceled) || len(r.inputs) != 1 {
			t.Fatal("continued after cancellation", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		r := newLayerRounds(t, layerRead)
		r.generateError = context.DeadlineExceeded
		_, err := r.run(t.Context())
		requireCode(t, err, "structured_generation_timeout")
	})
	for _, change := range []string{"model", "effort", "registry"} {
		t.Run("runtime-"+change, func(t *testing.T) {
			r := newLayerRounds(t, layerRead, layerFinal)
			original := r.options.Tools
			r.options.Tools = func(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
				outcomes, err := original(ctx, response)
				switch change {
				case "model":
					r.request.Model.ID = "changed"
				case "effort":
					r.request.Model.ReasoningEffort = "medium"
				case "registry":
					r.request.Tools = slices.Delete(r.request.Tools, 0, 1)
				}
				return outcomes, err
			}
			_, err := r.run(t.Context())
			requireCode(t, err, "structured_runtime_changed")
			if len(r.inputs) != 1 || len(r.calls) != 1 {
				t.Fatal("runtime changed mid-turn")
			}
		})
	}
}
