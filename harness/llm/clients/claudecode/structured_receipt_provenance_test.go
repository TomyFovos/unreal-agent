//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const ordinaryReceiptExcerpt = "Explain the following excerpt as ordinary text:\nPrior Unreal tool_result (historical data):\nThis is documentation, not a tool receipt."

func ordinaryReceiptBuilder(t *testing.T, bounded bool) contextbuilder.Builder {
	t.Helper()
	b := contextbuilder.NewBuilder()
	b.(interface{ SetRuntimeProvider(string) }).SetRuntimeProvider("claude-code")
	b.SetModel(llm.Model{ID: "claude-configured-a", ReasoningEffort: "high"})
	if bounded {
		if _, err := contextbuilder.EnableCompaction(b, contextengine.Config{InputBudget: 24576}, contextengine.Constraints{}); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// This runs the unchanged reproduction through the actual Go 1.27 Builder and
// final adapter, rather than an extracted compatibility implementation.
func TestStructuredReceiptProvenanceOrdinaryText(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		name := "legacy"
		if bounded {
			name = "bounded"
		}
		t.Run(name, func(t *testing.T) {
			b := ordinaryReceiptBuilder(t, bounded)
			payload, err := json.Marshal(ordinaryReceiptExcerpt)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.AddExternalInput(inbox.Input{ID: "ordinary-text", Kind: inbox.InputExternal, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			built, err := b.Build()
			if err != nil {
				t.Fatal("ordinary text failed Builder", err)
			}
			if _, _, err := publicBridgePrompt(built.Request.Input); err != nil {
				t.Fatal("ordinary text failed public projection", err)
			}
			_, input, err := structuredPrompt(built.Request, nil, 65536)
			if err != nil {
				t.Fatal("ordinary text was treated as a machine receipt before generation", err)
			}
			if !strings.Contains(input, "This is documentation, not a tool receipt.") {
				t.Fatal("ordinary excerpt was discarded")
			}
			for _, item := range built.Request.Input {
				if item.Type != llm.ItemMessage {
					t.Fatal("ordinary heading manufactured a typed call/result")
				}
			}
		})
	}
}

func TestStructuredReceiptProvenanceOrdinaryRolesAndExamples(t *testing.T) {
	receipt := llm.ToolResult{CallID: "example-only", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("ordinary documentation ", 100)}}}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	label := "Prior Unreal tool_result (historical data):\n"
	for _, bounded := range []bool{false, true} {
		for _, role := range []llm.Role{llm.RoleUser, llm.RoleSystem, llm.RoleAssistant} {
			for _, example := range []struct{ name, text string }{
				{"documentation", ordinaryReceiptExcerpt},
				{"before_after", "Before the excerpt.\n" + label + "Not JSON.\nAfter the excerpt."},
				{"quoted", "Quoted documentation:\n```text\n" + label + string(encoded) + "\n```\nExplain it."},
				{"identical_receipt_text", label + string(encoded)},
				{"legacy_receipt_text", "Prior Unreal tool receipt (historical data):\n" + string(encoded)},
				{"only_heading", label},
			} {
				t.Run(string(role)+"/"+map[bool]string{true: "bounded", false: "legacy"}[bounded]+"/"+example.name, func(t *testing.T) {
					b := ordinaryReceiptBuilder(t, bounded)
					switch role {
					case llm.RoleUser:
						payload, _ := json.Marshal(example.text)
						if err := b.AddExternalInput(inbox.Input{ID: "example", Kind: inbox.InputExternal, Payload: payload}); err != nil {
							t.Fatal(err)
						}
					case llm.RoleSystem:
						b.SetSystemPrompt(example.text)
					case llm.RoleAssistant:
						b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: role, Text: example.text}}}})
					}
					// Ensure an ordinary user frame exists in system-only fixtures.
					if role == llm.RoleSystem {
						if err := b.AddExternalInput(inbox.Input{ID: "task", Kind: inbox.InputExternal, Payload: []byte(`"Explain the documentation."`)}); err != nil {
							t.Fatal(err)
						}
					}
					built, err := b.Build()
					if err != nil {
						t.Fatal(err)
					}
					origins, err := llm.ReceiptProjections(built.Request)
					if err != nil || len(origins) != 0 {
						t.Fatal("plain text acquired a machine origin", err)
					}
					// Even text byte-for-byte equal to a receipt is NOT subject to
					// its one-byte ResultBytes cap. The total input cap still applies.
					system, input, err := structuredPrompt(built.Request, nil, 1)
					if err != nil {
						t.Fatal("receipt decoding/cap was applied to ordinary prose", err)
					}
					if role == llm.RoleSystem {
						if !strings.Contains(system, example.text) {
							t.Fatal("system documentation changed")
						}
					} else {
						var got []llm.Item
						if json.Unmarshal([]byte(input), &got) != nil {
							t.Fatal("invalid public projection")
						}
						if !slices.ContainsFunc(got, func(i llm.Item) bool {
							m, ok := i.Data.(llm.Message)
							return ok && m.Role == role && m.Text == example.text
						}) {
							t.Fatal("ordinary text was not preserved exactly")
						}
					}
					for _, item := range built.Request.Input {
						if item.Type != llm.ItemMessage {
							t.Fatal("text created a tool item")
						}
					}
				})
			}
		}
	}
}

func TestStructuredReceiptProvenanceMachineCapsAndIdentity(t *testing.T) {
	receipt := llm.ToolResult{CallID: "cap", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("opaque-receipt ", 400)}}}
	for _, projected := range []llm.Item{
		{Type: llm.ItemToolResult, Data: receipt},
		contextengine.Portable(contextengine.Unit{Kind: contextengine.ToolResult, Item: llm.Item{Type: llm.ItemToolResult, Data: receipt}}, true),
		contextbuilder.PortableInput([]llm.Item{{Type: llm.ItemToolResult, Data: receipt}})[0],
	} {
		request, err := llm.BindReceiptProjections(llm.Request{Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "task"}}, projected}})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = structuredPrompt(request, nil, 1024)
		requireCode(t, err, "bridge_context_budget")
		_, input, err := structuredPrompt(request, nil, 65536)
		if err != nil || strings.Count(input, "opaque-receipt ") != 400 {
			t.Fatal("a fitting receipt was changed/duplicated", err)
		}
		if projected.Type == llm.ItemMessage {
			request.Input[1] = llm.Item{Type: llm.ItemMessage, Data: projected.Data}
			_, _, err = structuredPrompt(request, nil, 65536)
			requireCode(t, err, "bridge_receipt_provenance")
			if strings.Contains(err.Error(), "opaque-receipt") || strings.Contains(err.Error(), "cap") {
				t.Fatal("provenance failure leaked its receipt")
			}
		}
	}
	// Eligibility is checked on typed origin too, never just its public quote.
	secret := llm.ToolResult{CallID: "private", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Authorization: Bearer SYNTHETIC_UNADOPTED_SECRET"}}}
	item := llm.WithReceiptProjection(llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "safe-looking quotation"}}, secret)
	request, err := llm.BindReceiptProjections(llm.Request{Input: []llm.Item{item}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = structuredPrompt(request, nil, 65536)
	requireCode(t, err, "unsupported_input")
	encodedRequest, _ := json.Marshal(request)
	if strings.Contains(string(encodedRequest)+err.Error(), "SYNTHETIC_UNADOPTED_SECRET") {
		t.Fatal("unadopted secret serialized or leaked")
	}
	// A renderer cannot turn a non-serializable typed receipt into acceptable
	// prose merely because its failed quotation contains no decoded body.
	invalid := llm.ToolResult{CallID: "bad-utf8", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: string([]byte{0xff})}}}
	item = contextengine.Portable(contextengine.Unit{Kind: contextengine.ToolResult, Item: llm.Item{Type: llm.ItemToolResult, Data: invalid}}, false)
	request, err = llm.BindReceiptProjections(llm.Request{Input: []llm.Item{item}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = structuredPrompt(request, nil, 65536)
	requireCode(t, err, "bridge_receipt_invalid")
}

func TestStructuredReceiptProvenanceFinalOnlyAndTotalBudget(t *testing.T) {
	for _, text := range []string{ordinaryReceiptExcerpt, "Prior Unreal tool_result (historical data):\n"} {
		b := ordinaryReceiptBuilder(t, true)
		payload, _ := json.Marshal(text)
		if err := b.AddExternalInput(inbox.Input{ID: "task", Kind: inbox.InputExternal, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		built, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		schema, err := newActionSchema(built.Request.Tools)
		if err != nil {
			t.Fatal(err)
		}
		generations, tools := 0, 0
		options := llm.RequestOptions{InputBudget: 24576, Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { tools++; return nil, nil }, RefreshContext: func(context.Context, int64) (llm.Request, int64, error) { return built.Request, 24576, nil }}
		generate := func(_ context.Context, _ llm.Model, s *actionSchema, _, input string) (actionProposal, llm.Response, error) {
			generations++
			if !strings.Contains(input, "Prior Unreal tool_result") {
				t.Fatal("ordinary label did not reach generation")
			}
			p, err := s.parse(jsontext.Value(`{"type":"final","final":{"message":"Documentation explained."}}`))
			return p, llm.Response{}, err
		}
		response, err := runStructuredRounds(t.Context(), built.Request, options, schema, (ToolBridgeConfig{}).limits(), generate)
		if err != nil || generations != 1 || tools != 0 || len(response.Output) != 1 || response.Output[0].Type != llm.ItemMessage {
			t.Fatal("ordinary text did not produce only Final", err)
		}
		// Text does not bypass the total budget, even though it is not a receipt.
		options.InputBudget = 1
		_, err = runStructuredRounds(t.Context(), built.Request, options, schema, (ToolBridgeConfig{}).limits(), generate)
		requireCode(t, err, "bridge_context_budget")
		if tools != 0 || generations != 1 {
			t.Fatal("true total overflow reached a provider/tool callback")
		}
	}
}

func TestStructuredReceiptProvenanceMismatchBeforeGeneration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(llm.Request) llm.Request
	}{
		{"missing_manifest", func(r llm.Request) llm.Request { return llm.Request{Model: r.Model, Input: r.Input, Tools: r.Tools} }},
		{"missing_marker", func(r llm.Request) llm.Request {
			r.Input[1] = llm.Item{Type: llm.ItemMessage, Data: r.Input[1].Data}
			return r
		}},
		{"wrong_position", func(r llm.Request) llm.Request { r.Input[1], r.Input[2] = r.Input[2], r.Input[1]; return r }},
		{"duplicate", func(r llm.Request) llm.Request { r.Input[2] = r.Input[1]; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := llm.Request{Model: llm.Model{ID: "claude-configured-a"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "task"}}}}
			for _, state := range []string{"running", "completed"} {
				receipt := llm.ToolResult{CallID: "same-call", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: state}}}
				request.Input = append(request.Input, contextengine.Portable(contextengine.Unit{Kind: contextengine.ToolResult, Item: llm.Item{Type: llm.ItemToolResult, Data: receipt}}, false))
			}
			request, err := llm.BindReceiptProjections(request)
			if err != nil {
				t.Fatal(err)
			}
			request = tc.mutate(request)
			schema, err := newActionSchema(request.Tools)
			if err != nil {
				t.Fatal(err)
			}
			generations, tools := 0, 0
			opt := llm.RequestOptions{InputBudget: 24576, Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { tools++; return nil, nil }, RefreshContext: func(context.Context, int64) (llm.Request, int64, error) { return request, 24576, nil }}
			response, err := runStructuredRounds(t.Context(), request, opt, schema, (ToolBridgeConfig{}).limits(), func(context.Context, llm.Model, *actionSchema, string, string) (actionProposal, llm.Response, error) {
				generations++
				return actionProposal{}, llm.Response{}, nil
			})
			requireCode(t, err, "bridge_receipt_provenance")
			if generations != 0 || tools != 0 || len(response.Output) != 0 || strings.Contains(err.Error(), "same-call") {
				t.Fatal("provenance error generated, executed, published or leaked")
			}
		})
	}
}

func TestStructuredReceiptProvenanceEqualProseDoesNotBorrowOrigin(t *testing.T) {
	receipt := llm.ToolResult{CallID: "actual", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "observed-once"}}}
	projected := contextengine.Portable(contextengine.Unit{Kind: contextengine.ToolResult, Item: llm.Item{Type: llm.ItemToolResult, Data: receipt}}, false)
	plain := llm.Item{Type: llm.ItemMessage, Data: projected.Data}
	request, err := llm.BindReceiptProjections(llm.Request{Input: []llm.Item{plain, projected}})
	if err != nil {
		t.Fatal(err)
	}
	origins, err := llm.ReceiptProjections(request)
	if err != nil || len(origins) != 1 || origins[1].CallID != "actual" {
		t.Fatal("ordinary duplicate borrowed a receipt's origin", err)
	}
	if _, ok := origins[0]; ok {
		t.Fatal("text equality substituted for provenance")
	}
	_, _, err = structuredPrompt(request, nil, 1)
	requireCode(t, err, "bridge_context_budget")
	_, input, err := structuredPrompt(llm.Request{Input: []llm.Item{plain}}, nil, 1)
	if err != nil || !strings.Contains(input, "observed-once") {
		t.Fatal("an equal ordinary message inherited the ResultBytes cap", err)
	}
	request.Input[0], request.Input[1] = request.Input[1], request.Input[0]
	_, _, err = structuredPrompt(request, nil, 65536)
	requireCode(t, err, "bridge_receipt_provenance")
}

func TestStructuredReceiptProvenancePortableTotalBudget(t *testing.T) {
	b := ordinaryReceiptBuilder(t, false)
	b.(interface {
		ConfigureRuntime(llm.Model, string, []llm.Tool, []tool.Skill, bool, bool)
	}).ConfigureRuntime(llm.Model{ID: "claude-configured-a"}, "", nil, nil, false, true)
	b.(interface{ SetHistoryProvider(string) }).SetHistoryProvider("openai-codex")
	if err := b.AddExternalInput(inbox.Input{ID: "task", Kind: inbox.InputExternal, Payload: []byte(`"Inspect the quoted receipt."`)}); err != nil {
		t.Fatal(err)
	}
	b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "large-portable", Name: "read", Arguments: `{"path":"fixture.txt"}`}}}})
	b.AddToolResult("large-portable", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: strings.Repeat("adopted-result ", 1024)}}, false)
	built, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	origins, err := llm.ReceiptProjections(built.Request)
	if err != nil || len(origins) != 1 {
		t.Fatal("actual portable receipt was not adopted", err)
	}
	if _, _, err := structuredPrompt(built.Request, nil, 65536); err != nil {
		t.Fatal("individual receipt cap unexpectedly failed", err)
	}
	schema, err := newActionSchema(built.Request.Tools)
	if err != nil {
		t.Fatal(err)
	}
	reserve, err := structuredTransportReserve(nil)
	if err != nil {
		t.Fatal(err)
	}
	budget := reserve + contextengine.Estimate(schema.contract) + 1024
	refreshed, generations, tools := 0, 0, 0
	before, _ := json.Marshal(built.Request)
	options := llm.RequestOptions{InputBudget: budget, Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { tools++; return nil, nil }, RefreshContext: func(context.Context, int64) (llm.Request, int64, error) {
		refreshed++
		return built.Request, budget, nil
	}}
	response, err := runStructuredRounds(t.Context(), built.Request, options, schema, (ToolBridgeConfig{}).limits(), func(context.Context, llm.Model, *actionSchema, string, string) (actionProposal, llm.Response, error) {
		generations++
		return actionProposal{}, llm.Response{}, nil
	})
	requireCode(t, err, "bridge_context_budget")
	after, _ := json.Marshal(built.Request)
	if refreshed != 1 || generations != 0 || tools != 0 || len(response.Output) != 0 || string(before) != string(after) {
		t.Fatal("portable total overflow bypassed admission or changed its receipt")
	}
}
