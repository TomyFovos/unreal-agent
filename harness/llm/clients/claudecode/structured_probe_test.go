//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

var structuredProbeLevels = flag.Int("claude-structured-probe-levels", 0, "explicit real inference opt-in: 1=minimal, 5=full schema ladder; default skips")
var structuredProbeExactLevel = flag.Int("claude-structured-probe-exact-level", 0, "explicit real inference opt-in: run only this schema level (1-5); mutually exclusive with probe-levels")
var structuredProbeVariant = flag.String("claude-structured-probe-variant", "", "explicit real inference opt-in: run one schema variant A-F/C1/C2/T1-T7; mutually exclusive with both level modes")
var structuredProbeBinary = flag.String("claude-structured-probe-binary", "", "absolute external Claude CLI path for the opt-in adapter probe")
var structuredProbeModel = flag.String("claude-structured-probe-model", "", "explicit selectable Claude model alias for the opt-in adapter probe")

// Exactly the requested minimal schema: no oneOf, registry, tool, filesystem,
// Host, permission grant, Session or Operation. Larger levels change only schema.
const structuredMinimalProbeSchema = `{"type":"object","properties":{"type":{"const":"final"},"final":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"],"additionalProperties":false}},"required":["type","final"],"additionalProperties":false}`

// An inert schema witness, not a Registry tool, MCP name or executor. Only the
// generic Level 2 probe can validate this name, always without any execution.
const structuredProbeGenericTool = "probe_proposal_only"

func structuredProbeSchema(t *testing.T, level int) *actionSchema {
	t.Helper()
	if level >= 3 {
		tools := layerRegistryTools(t)
		if level < 5 {
			names := []string{tool.ReadName}
			if level == 4 {
				names = append(names, tool.GlobName, tool.GrepName)
			}
			tools = slices.DeleteFunc(tools, func(t llm.Tool) bool { return !slices.Contains(names, t.Name) })
		}
		schema, err := newActionSchema(tools)
		if err != nil {
			t.Fatal("probe schema compile failed")
		}
		// Preserve historical Level 3-5 wire definitions for independent schema
		// audits; the new transport has its own T7 probe. Never silently change a
		// recorded level/hash when production's serialization shape changes.
		schema.transport = nil
		return schema
	}
	document := jsontext.Value(structuredMinimalProbeSchema)
	if level == 2 {
		var final map[string]any
		if json.Unmarshal(document, &final) != nil {
			t.Fatal("minimal fixture malformed")
		}
		// Generic proposal shape for serializer complexity only. No name is
		// registered and no Action is authorized/executed by the probe.
		action := map[string]any{"type": "object", "properties": map[string]any{"type": map[string]any{"const": "action"}, "action": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": "^[A-Za-z0-9_-]+$"}, "tool": map[string]any{"type": "string"}, "arguments": map[string]any{"type": "object", "additionalProperties": false}}, "required": []string{"id", "tool", "arguments"}, "additionalProperties": false}}, "required": []string{"type", "action"}, "additionalProperties": false}
		var err error
		document, err = json.Marshal(map[string]any{"type": "object", "oneOf": []any{final, action}}, json.Deterministic(true))
		if err != nil {
			t.Fatal("generic schema fixture malformed")
		}
	}
	validator, err := compileStructuredDocument(document)
	if err != nil {
		t.Fatal("minimal probe schema compile failed")
	}
	schema := &actionSchema{document: document, validator: validator, tools: map[string]bool{}}
	if level == 2 {
		// The wire schema remains the existing generic branch. Its local probe
		// validator permits a single inert witness; unknown names remain rejected.
		// No production validator, Registry definition or tool capability changes.
		schema.tools[structuredProbeGenericTool] = true
		schema.arguments = map[string]*jsonschema.Schema{structuredProbeGenericTool: validator.OneOf[1].Properties["action"].Properties["arguments"]}
	}
	return schema
}

func TestStructuredLayerProbeSchemaLadder(t *testing.T) {
	for level := 1; level <= 5; level++ {
		schema := structuredProbeSchema(t, level)
		if schema.validator.DraftVersion != 7 {
			t.Fatal("probe changed Draft")
		}
		if _, err := schema.parse(jsontext.Value(layerFinal)); err != nil {
			t.Fatal("probe Final invalid", level, err)
		}
		if level <= 2 {
			structuredGolden(t, []string{"probe-minimal.schema.json", "probe-generic.schema.json"}[level-1], schema.document)
		}
		want := 0
		switch level {
		case 1:
			if !bytes.Equal(schema.document, []byte(structuredMinimalProbeSchema)) || len(schema.validator.OneOf) != 0 {
				t.Fatal("minimal schema was expanded")
			}
		case 2:
			want = 1 // one inert validation witness; no Registry/execution tool
			if len(schema.validator.OneOf) != 2 {
				t.Fatal("generic branch absent")
			}
		case 3:
			want = 1
		case 4:
			want = 3
		case 5:
			want = len(layerRegistryTools(t))
		}
		if len(schema.tools) != want {
			t.Fatal("schema level tool count", level, len(schema.tools), want)
		}
		if level >= 3 {
			if _, err := schema.parse(jsontext.Value(layerRead)); err != nil {
				t.Fatal("registry Action rejected", level, err)
			}
		}
	}
}

// Ordinary go test, -race and make targets SKIP this test. Only an explicit
// user-run flag starts real inference. Ladder mode begins at Level 1; exact mode
// visits only its selected level; variant mode uses the existing offline A-F
// builder once. Failure stops without retry, fallback, execution or AIdea E2E.
func TestStructuredRealAdapterSchemaProbe(t *testing.T) {
	selection, err := structuredProbeSelectionFromFlags(flag.CommandLine, *structuredProbeLevels, *structuredProbeExactLevel, *structuredProbeVariant)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Levels) == 0 && selection.Variant == "" {
		t.Skip("real Claude inference disabled; explicit adapter-probe flags required")
	}
	if !filepath.IsAbs(*structuredProbeBinary) || !publicID(*structuredProbeModel) {
		t.Fatal("invalid explicit adapter-probe configuration")
	}
	c, err := NewClient(Config{Binary: *structuredProbeBinary, ManagedPolicyMode: ManagedPolicyTrust, ToolBridge: ToolBridgeConfig{Enabled: true, Mode: BridgeModeStructured}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	path, env, err := c.prepare(ctx)
	cancel()
	if err != nil {
		t.Fatal("non-inference authentication/isolation preflight", err)
	}
	model := llm.Model{ID: *structuredProbeModel, ReasoningEffort: llm.ReasoningEffortHigh}
	var reports []structuredProbeReport
	if selection.Variant != "" {
		variant := selectedProbeSchemaVariant(t, selection.Variant)
		reports = []structuredProbeReport{runStructuredProbeVariant(t.Context(), variant, model,
			func(ctx context.Context, invocation structuredProbeInvocation, variant probeSchemaVariant) structuredProbeDiagnostic {
				return structuredProbeVariantGeneration(ctx, path, env, invocation, variant)
			})}
	} else {
		reports = runStructuredProbeSequence(t.Context(), selection.Levels, model,
			func(level int) *actionSchema { return structuredProbeSchema(t, level) },
			func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
				return c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
			})
	}
	for _, report := range reports {
		selector := fmt.Sprintf("level=%d", report.Level)
		if report.Variant != "" {
			selector = "variant=" + report.Variant
		}
		if !report.Diagnostic.accepted() {
			t.Fatalf("adapter schema %s FAIL; schema_bytes=%d; schema_sha256=%s; %s; execution=0; MCP=0; Operations=0",
				selector, report.SchemaBytes, report.SchemaSHA256, report.Diagnostic.summary())
		}
		t.Logf("adapter schema %s PASS; schema_bytes=%d; schema_sha256=%s; %s; execution=0; MCP=0; Operations=0",
			selector, report.SchemaBytes, report.SchemaSHA256, report.Diagnostic.summary())
	}
}

func structuredProbeSelectionFromFlags(flags *flag.FlagSet, ladder, exact int, variant string) (structuredProbeSelection, error) {
	selection, err := selectStructuredProbe(ladder, exact, variant)
	if err != nil || variant == "" {
		return selection, err
	}
	// Even explicitly specifying a disabled (=0) level flag alongside a
	// variant is ambiguous. Reject before authentication or process creation.
	conflict := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "claude-structured-probe-levels" || f.Name == "claude-structured-probe-exact-level" {
			conflict = true
		}
	})
	if conflict {
		return structuredProbeSelection{}, errors.New("adapter probe variant cannot be combined with either level flag")
	}
	return selection, nil
}
