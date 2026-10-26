package contextbuilder

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestProjectionBuildAvoidsRepeatedMarginalMeasures(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{})
	f.turn(t, "prior", "synthetic prior task", "synthetic prior answer")
	f.user(t, "current", "synthetic current task")
	counts := map[string]int{}
	full := 0
	projection := llm.ContextProjection{Measure: func(r llm.Request) (int64, error) {
		data, err := json.Marshal(r, json.Deterministic(true))
		if err != nil {
			return 0, err
		}
		if len(r.Input) > 0 && strings.Contains(contextengine.Text(r.Input[0]), "required runtime instructions") {
			full++
		} else {
			counts[string(data)]++
		}
		return contextengine.Estimate(r), nil
	}}
	if _, err := f.b.BuildWithProjection(projection); err != nil {
		t.Fatal(err)
	}
	// The complete selected projection and final renderer must BOTH execute.
	if full < 3 {
		t.Fatal("base/selected/final validation was skipped", full)
	}
	for _, n := range counts {
		if n > 2 {
			t.Fatal("repeated immutable marginal projection", n)
		}
	}
}

// The original marginal algorithm, kept only as a reference in tests.
func referenceProjectionCost(measure func([]llm.Item) (int64, error), key projectionCostKey, render func() []llm.Item) (int64, error) {
	var partner []llm.Item
	if key.partner.present {
		partner = []llm.Item{{Type: llm.ItemToolCall, Data: key.partner.call}}
	}
	base, err := measure(partner)
	if err != nil {
		return 0, err
	}
	value, err := measure(append(slices.Clone(partner), render()...))
	if err != nil {
		return 0, err
	}
	if value < base {
		return 0, &contextengine.Error{Code: "invalid_projection_cost"}
	}
	return value - base + 2, nil
}

func TestProjectionMemoTypedCandidateIdentity(t *testing.T) {
	u := contextengine.Unit{ID: "unit-1", Kind: contextengine.ToolResult, Class: contextengine.Keep,
		Source: contextengine.HistoryRef{Sequence: 3}, Group: "response-1", Origin: "claude-code"}
	base := projectionKey(u, "recent", false, false)
	base.partner = projectionPartnerKey{present: true, call: llm.ToolCall{CallID: "same-call", Name: "read", Arguments: `{"path":"fixture.txt"}`}}
	cases := map[string]func(*projectionCostKey){
		"same body different unit": func(k *projectionCostKey) { k.unit.id = "unit-2" },
		"running/final":            func(k *projectionCostKey) { k.unit.id, k.unit.source.Sequence = "unit-3", 4 },
		"reason":                   func(k *projectionCostKey) { k.reason = "retrieved" },
		"reference":                func(k *projectionCostKey) { k.reference = true },
		"native/portable retry":    func(k *projectionCostKey) { k.quoted = true },
		"required group copy":      func(k *projectionCostKey) { k.unit.required, k.unit.class = true, contextengine.Reference },
		"retrieval-only":           func(k *projectionCostKey) { k.unit.retrievalOnly = true },
		"source range":             func(k *projectionCostKey) { k.unit.source.StartByte, k.unit.source.EndByte = 1, 9 },
		"origin":                   func(k *projectionCostKey) { k.unit.origin = "openai-codex" },
		"partner arguments":        func(k *projectionCostKey) { k.partner.call.Arguments = `{"path":"other.txt"}` },
		"partner name":             func(k *projectionCostKey) { k.partner.call.Name = "glob" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			calls, rendered := 0, 0
			measure := func(items []llm.Item) (int64, error) { calls++; return contextengine.Estimate(items), nil }
			memo := newProjectionMemo(measure, 2)
			key := base
			for i := 0; i < 2; i++ {
				if i == 1 {
					change(&key)
				}
				items := []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: fmt.Sprint("body-", i)}}}
				render := func() []llm.Item { rendered++; return items }
				want, err := referenceProjectionCost(measure, key, func() []llm.Item { return items })
				if err != nil {
					t.Fatal(err)
				}
				got, err := memo.cost(key, render)
				if err != nil || got != want {
					t.Fatal("reference marginal size changed", got, want, err)
				}
				before := calls
				again, err := memo.cost(key, render)
				if err != nil || again != want || calls != before || rendered != i+1 {
					t.Fatal("same immutable candidate recomputed or wrong candidate reused")
				}
			}
			if len(memo.costs) != 2 {
				t.Fatal("candidate states aliased")
			}
		})
	}
}

func TestProjectionMemoSuccessfulResultsOnlyAndBounds(t *testing.T) {
	for _, where := range []string{"base", "candidate", "negative marginal"} {
		t.Run(where, func(t *testing.T) {
			failure := &contextengine.Error{Code: "synthetic_projection_failure"}
			calls := 0
			measure := func(items []llm.Item) (int64, error) {
				calls++
				if where == "base" || where == "candidate" && len(items) > 0 {
					return 0, failure
				}
				if len(items) == 0 {
					return 10, nil
				}
				return 5, nil
			}
			key := projectionKey(contextengine.Unit{ID: "unit-1"}, "recent", false, false)
			render := func() []llm.Item { return []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "body"}}} }
			memo := newProjectionMemo(measure, 0)
			_, want := referenceProjectionCost(measure, key, render)
			for range 2 {
				before := calls
				_, err := memo.cost(key, render)
				var actual, expected *contextengine.Error
				if !errors.As(err, &actual) || !errors.As(want, &expected) || actual.Code != expected.Code || calls == before || len(memo.costs) != 0 {
					t.Fatal("error was cached or changed", err, want)
				}
			}
			if where == "base" && len(memo.bases) != 0 {
				t.Fatal("failed base cached")
			}
		})
	}
	measure := func(items []llm.Item) (int64, error) { return contextengine.Estimate(items), nil }
	memo := newProjectionMemo(measure, 0)
	for i := range 100 {
		key := projectionKey(contextengine.Unit{ID: "unit-1"}, fmt.Sprint(i), false, false)
		key.partner = projectionPartnerKey{present: true, call: llm.ToolCall{CallID: fmt.Sprint(i)}}
		_, err := memo.cost(key, func() []llm.Item { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(memo.costs) != 16 || len(memo.bases) != 1 {
		t.Fatal("memo is not bounded by Units/partners")
	}
}

func TestProjectionBuildMemoLifetimeAndFinalValidation(t *testing.T) {
	f := newContextFixture(t, "claude-code", contextengine.Config{})
	f.user(t, "task", "synthetic current task")
	calls := 0
	projection := llm.ContextProjection{Measure: func(r llm.Request) (int64, error) { calls++; return contextengine.Estimate(r), nil }}
	one, err := f.b.BuildWithProjection(projection)
	if err != nil {
		t.Fatal(err)
	}
	firstCalls := calls
	two, err := f.b.BuildWithProjection(projection)
	if err != nil || calls != 2*firstCalls {
		t.Fatal("memo survived Build", calls, firstCalls, err)
	}
	one.Report.Context.RetrievalMicros, two.Report.Context.RetrievalMicros = 0, 0
	if !reflect.DeepEqual(one, two) {
		t.Fatal("Build changed semantic output")
	}
	// Model/schema/receipt framing changes on the next generation and must be
	// observed rather than using a Session/global memo.
	projection.Measure = func(r llm.Request) (int64, error) { return 100 + contextengine.Estimate(r), nil }
	three, err := f.b.BuildWithProjection(projection)
	if err != nil || three.Report.Context.EstimatedInputTokens != one.Report.Context.EstimatedInputTokens+100 {
		t.Fatal("generation reused an old size", err)
	}
	for _, fail := range []bool{true, false} {
		full := 0
		projection.Measure = func(r llm.Request) (int64, error) {
			if len(r.Input) > 1 && r.Input[0].Data.(llm.Message).Role == llm.RoleSystem {
				full++
				if full == 2 {
					if fail {
						return 0, &contextengine.Error{Code: "synthetic_final_failure"}
					}
					return 1 << 60, nil
				}
			}
			return contextengine.Estimate(r), nil
		}
		_, err := f.b.BuildWithProjection(projection)
		var typed *contextengine.Error
		want := "required_context_exceeds_budget"
		if fail {
			want = "synthetic_final_failure"
		}
		if !errors.As(err, &typed) || typed.Code != want || full != 2 {
			t.Fatal("final error/budget validation skipped", full, err)
		}
	}
}
