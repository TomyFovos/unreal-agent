//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func structuredProbeWitness(t *testing.T, level int) string {
	t.Helper()
	if level == 1 {
		return structuredProbeFinal
	}
	name, arguments := structuredProbeGenericTool, any(map[string]any{})
	if level >= 3 {
		name = tool.ReadName
		if level == 4 {
			name = tool.GlobName
		}
		for _, registered := range layerRegistryTools(t) {
			if registered.Name == name {
				arguments = layerSchemaWitness(t, registered.Parameters)
				break
			}
		}
	}
	raw, err := json.Marshal(map[string]any{"type": "action", "action": map[string]any{"id": "probe_witness", "tool": name, "arguments": arguments}}, json.Deterministic(true))
	if err != nil {
		t.Fatal("synthetic schema witness could not be encoded")
	}
	return string(raw)
}

func TestStructuredLayerProbeExactSelection(t *testing.T) {
	for _, tc := range []struct {
		ladder, exact int
		want          []int
		invalid       bool
	}{
		{0, 0, nil, false},
		{1, 0, []int{1}, false},
		{5, 0, []int{1, 2, 3, 4, 5}, false},
		{0, 1, []int{1}, false},
		{0, 2, []int{2}, false},
		{0, 3, []int{3}, false},
		{0, 4, []int{4}, false},
		{0, 5, []int{5}, false},
		{-1, 0, nil, true},
		{6, 0, nil, true},
		{0, -1, nil, true},
		{0, 6, nil, true},
		{5, 2, nil, true},
		{1, 1, nil, true},
	} {
		t.Run(fmt.Sprintf("ladder-%d-exact-%d", tc.ladder, tc.exact), func(t *testing.T) {
			got, err := selectStructuredProbeLevels(tc.ladder, tc.exact)
			if (err != nil) != tc.invalid || !slices.Equal(got, tc.want) {
				t.Fatal("probe selection did not isolate its exact level")
			}
		})
	}
	for _, name := range []string{"claude-structured-probe-levels", "claude-structured-probe-exact-level"} {
		registered := flag.Lookup(name)
		if registered == nil || registered.DefValue != "0" {
			t.Fatal("real probe opt-in flag missing or enabled by default")
		}
	}
}

func TestStructuredLayerProbeExactRequestMatchesLadder(t *testing.T) {
	c := &Client{config: Config{ManagedPolicyMode: ManagedPolicyTrust, Getenv: func(key string) string {
		if key == "HOME" {
			return "/synthetic-home"
		}
		return ""
	}}}
	env, err := c.environment()
	if err != nil {
		t.Fatal(err)
	}
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	run := func(levels []int) ([]probeRequestSnapshot, []structuredProbeReport) {
		var requests []probeRequestSnapshot
		built := 0
		reports := runStructuredProbeSequence(t.Context(), levels, model,
			func(level int) *actionSchema {
				if built >= len(levels) || level != levels[built] {
					t.Fatal("built a previous or unselected schema level")
				}
				built++
				return structuredProbeSchema(t, level)
			},
			func(ctx context.Context, got llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > structuredProbeTimeout {
					t.Fatal("exact-mode timeout changed")
				}
				request, err := buildStructuredRequest(got, env, schema, "/private-temp/system.txt", input, false)
				if err != nil {
					t.Fatal(err)
				}
				requests = append(requests, probeRequestSnapshot{Model: got, System: system, Input: input, Timeout: structuredProbeTimeout, Request: request})
				var stdin bytes.Buffer
				return runStructuredProtocol(strings.NewReader(structuredProbeStream(structuredProbeFinal)), &stdin, schema, input, false)
			})
		if built != len(levels) || len(requests) != len(levels) || len(reports) != len(levels) {
			t.Fatal("probe builder/generator count changed")
		}
		return requests, reports
	}
	ladder, _ := selectStructuredProbeLevels(5, 0)
	requests, reports := run(ladder)
	for level := 1; level <= 5; level++ {
		t.Run(fmt.Sprintf("level-%d", level), func(t *testing.T) {
			exact, err := selectStructuredProbeLevels(0, level)
			if err != nil {
				t.Fatal(err)
			}
			one, single := run(exact)
			if !reflect.DeepEqual(one[0], requests[level-1]) || !reflect.DeepEqual(single[0], reports[level-1]) || !single[0].Diagnostic.accepted() {
				t.Fatal("exact and ladder schema/request/outcome differ")
			}
			t.Logf("exact == ladder; level=%d schema_bytes=%d schema_sha256=%s", level, single[0].SchemaBytes, single[0].SchemaSHA256)
		})
	}
}

func TestStructuredLayerProbeExactEnvelopeMatrix(t *testing.T) {
	for level := 1; level <= 5; level++ {
		t.Run(fmt.Sprintf("level-%d", level), func(t *testing.T) {
			schema := structuredProbeSchema(t, level)
			for _, raw := range []string{structuredProbeFinal, structuredProbeWitness(t, level)} {
				var stdin bytes.Buffer
				p, response, err := runStructuredProtocol(strings.NewReader(structuredProbeStream(raw)), &stdin, schema, structuredProbeInput, false)
				d := classifyStructuredProbeLevel(level, p, response, err)
				want := probeValidatedFinal
				if level >= 2 && raw != structuredProbeFinal {
					want = probeValidatedAction
				}
				if d.Outcome != want || !d.accepted() || !d.SchemaChecked || !d.SchemaValid || len(response.Output) != 0 {
					t.Fatal("valid level envelope was rejected or became public output", d.summary())
				}
			}
			for _, raw := range []string{
				`{"type":"action","action":{"id":"probe","tool":"private-probe-unknown-tool","arguments":{}}}`,
				`{"type":"action","action":{"id":"probe","tool":"` + structuredProbeGenericTool + `","arguments":{"raw":"private-probe-arguments"}}}`,
				`{"type":"final","final":{"message":"private-probe-prose"}}`,
				`{"type":"final"}`, `null`,
			} {
				var stdin bytes.Buffer
				p, response, err := runStructuredProtocol(strings.NewReader(structuredProbeStream(raw)), &stdin, schema, structuredProbeInput, false)
				d := classifyStructuredProbeLevel(level, p, response, err)
				if d.accepted() || strings.Contains(d.summary(), "private-probe-") || len(response.Output) != 0 {
					t.Fatal("invalid/unexpected envelope was accepted or leaked")
				}
			}
		})
	}
	// Generic Level 2 has exactly one inert local validation witness. It cannot
	// impersonate a Registry tool, built-in executor or foreign MCP endpoint.
	schema := structuredProbeSchema(t, 2)
	for _, name := range []string{"Bash", "Read", "read", "Edit", "Agent", "mcp__foreign__read"} {
		raw, _ := json.Marshal(map[string]any{"type": "action", "action": map[string]any{"id": "probe", "tool": name, "arguments": map[string]any{}}})
		_, err := schema.parse(jsontext.Value(raw))
		requireCode(t, err, "bridge_unknown_tool")
	}
}
