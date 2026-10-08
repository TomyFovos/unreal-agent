//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// T1-T6 add one transport feature per step. T7 uses the production candidate
// built from a frozen Registry snapshot, and validates its execution contract
// separately. The shared probe still returns only diagnostic enums, never work.
func probeTransportVariant(t *testing.T, level int) probeSchemaVariant {
	t.Helper()
	closed := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	}
	action := closed(map[string]any{})
	final := closed(map[string]any{"message": map[string]any{"type": "string"}})
	final["required"] = []string{"message"}
	document := closed(map[string]any{"type": map[string]any{"type": "string"}, "final": final, "action": action})
	props := document["properties"].(map[string]any)
	fields := action["properties"].(map[string]any)
	if level >= 2 {
		props["type"] = map[string]any{"type": "string", "enum": []string{"final", "action"}}
	}
	if level >= 3 {
		fields["id"] = map[string]any{"type": "string"}
	}
	if level >= 4 {
		fields["tool"] = map[string]any{"type": "string"}
	}
	if level >= 5 {
		fields["arguments"] = closed(map[string]any{})
	}
	if level >= 6 {
		document["required"], action["required"] = []string{"type"}, []string{"id", "tool", "arguments"}
	}
	raw, err := json.Marshal(document, json.Deterministic(true))
	if err != nil {
		t.Fatal("transport probe encoding failed")
	}
	var strict *actionSchema
	if level == 7 {
		strict, err = newActionSchema(layerRegistryTools(t))
		if err == nil {
			raw, err = structuredTransportDocument(strict.document)
		}
	}
	if err != nil {
		t.Fatal("transport candidate invalid", err)
	}
	validator, err := compileStructuredDocument(raw)
	if err != nil {
		t.Fatal("transport probe Draft 7 compile failed", err)
	}
	return probeSchemaVariant{Name: fmt.Sprintf("T%d", level), Schema: &actionSchema{document: raw, validator: validator, tools: map[string]bool{}}, Strict: strict}
}

func TestStructuredLayerTransportVariants(t *testing.T) {
	hashes := []string{
		"addf8d177956663819cd4b0e76e96c15e90082800fa69f8e2cb5e0a8ce0c3516",
		"bf51936c706404535bec4aa6a711fb0e3224bfde9e43bd9a44a40f1d73dee564",
		"ec55f6c4c4c0f87bce954275cb0fa90c6dcc9c852c3c8ad7b2909d9539113a71",
		"9e12c61e7ea6bbd9636b452e23d23f7ad01a06fd25a8550d8241a0b12f13a320",
		"8a06c10b2adae8f96f5074f491eb2c061c7a166a8fb8c13d163dff89ce8343c6",
		"e86b3319e668869f8213f7ca0a18c38f50773933fd99108a2cc6eb525e6358c4",
		"189b395f91c1f77b46c5846c7c0be7725f65030cec11f6226855176d82001e80",
	}
	counts := []int{279, 305, 327, 352, 427, 484, 1910}
	for level := 1; level <= 7; level++ {
		v := probeTransportVariant(t, level)
		t.Run(v.Name, func(t *testing.T) {
			if len(v.Schema.document) != counts[level-1] || fmt.Sprintf("%x", sha256.Sum256(v.Schema.document)) != hashes[level-1] {
				t.Fatal("transport probe bytes/hash changed")
			}
			inspection := inspectProbeSchema(t, v.Schema.document)
			if v.Schema.validator.DraftVersion != 7 || len(inspection.StrictUnsupported) != 0 {
				t.Fatal("transport candidate added an unsupported strict keyword", inspection.StrictUnsupported)
			}
			for _, key := range []string{"oneOf", "anyOf", "allOf", "$ref", "$defs", "definitions", "pattern", "minLength", "maxLength"} {
				if inspection.Keywords[key] != 0 {
					t.Fatal("unexpected keyword in transport", key)
				}
			}
			if d := runStructuredProbeVariantFixture(t, v, structuredProbeStream(structuredProbeFinal)); !d.accepted() || d.Outcome != probeValidatedFinal {
				t.Fatal("transport Final rejected", d.summary())
			}
			selection, err := selectStructuredProbe(0, 0, v.Name)
			selected := selectedProbeSchemaVariant(t, selection.Variant)
			model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
			invocation := newStructuredProbeInvocation(model, v.Schema)
			baseline, buildErr := buildStructuredRequest(model, []string{"HOME=/synthetic-home"}, v.Schema, "/private-temp/system.txt", invocation.Input, false)
			got, selectedErr := buildStructuredRequest(model, []string{"HOME=/synthetic-home"}, selected.Schema, "/private-temp/system.txt", invocation.Input, false)
			if err != nil || buildErr != nil || selectedErr != nil || len(selection.Levels) != 0 || !reflect.DeepEqual(got, baseline) || !bytes.Equal(selected.Schema.document, v.Schema.document) {
				t.Fatal("transport selector/offline request differs")
			}
			for _, invalid := range []string{`{"type":"final","final":{"message":"adapter schema probe OK"},"unexpected":true}`, `{"type":"final","final":{"message":"adapter schema probe OK","unexpected":true}}`} {
				if validateStructuredProbeVariantOutput(v, jsontext.Value(invalid)).accepted() {
					t.Fatal("transport unknown field accepted")
				}
			}
			t.Logf("variant=%s schema_bytes=%d schema_sha256=%x", v.Name, len(v.Schema.document), sha256.Sum256(v.Schema.document))
		})
	}
}

func TestStructuredLayerTransportProductionPair(t *testing.T) {
	v := probeTransportVariant(t, 7)
	s := v.Strict
	if bytes.Equal(s.document, s.transport) || !bytes.Equal(s.transport, v.Schema.document) || !bytes.Equal(s.requestDocument(), s.transport) || len(s.validator.OneOf) != len(layerRegistryTools(t))+1 {
		t.Fatal("production transport replaced or changed the authoritative validator")
	}
	structuredGolden(t, "registry.schema.json", s.document) // Previous semantic schema stays byte-identical.
	for _, raw := range []string{
		`{"type":"action","action":{"id":"a","tool":"Bash","arguments":{"command":"private-probe-command","path":"private-probe-path"}}}`,
		`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"private-probe-path","command":"private-probe-command"}}}`,
		`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":123}}}`,
		`{"type":"action","action":{"id":"a","tool":"missing","arguments":{}}}`,
		`{"type":"final","final":{"message":"private-probe-message"},"action":null}`,
		`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"private-probe-path"}},"final":null}`,
		`{"type":"final","final":{"message":"private-probe-message","unknown":true}}`,
		`{"type":"action","action":{"id":"bad/id","tool":"read","arguments":{"path":"private-probe-path"}}}`,
		`{"type":"final","final":{"message":"private-probe-message"},"action":{"id":"a","tool":"read","arguments":{"path":"private-probe-path"}}}`,
		`{"type":"unknown"}`, `{"type":"final"}`, `{"type":"action"}`, `{}`,
	} {
		p, err := s.parse(jsontext.Value(raw))
		if err == nil || p.Action != nil || p.Final != nil {
			t.Fatal("invalid transport result became an execution/public proposal")
		}
		assertProbeFailureValueFree(t, classifyStructuredProbe(p, llm.Response{}, err), err)
	}
	updated, err := newActionSchema(layerRegistryTools(t))
	if err != nil || !sameSchema(s, updated) {
		t.Fatal("production schema pair is nondeterministic")
	}
	updated.transport = append(jsontext.Value(nil), updated.transport...)
	updated.transport[0] = ' '
	if sameSchema(s, updated) {
		t.Fatal("runtime boundary ignored a transport change")
	}
	if !strings.Contains(s.contract, "omit action entirely") || !strings.Contains(s.contract, "omit final entirely") || !strings.Contains(s.contract, `"argumentsSchema"`) || !strings.Contains(s.contract, `"name":"SubagentStart"`) {
		t.Fatal("private protocol lost per-tool/exclusive branch requirements")
	}
}

func TestStructuredLayerTransportContractReserve(t *testing.T) {
	r := newLayerRounds(t, layerRead, layerFinal)
	var reserves []int64
	r.options.RefreshContext = func(_ context.Context, reserve int64) (llm.Request, int64, error) {
		reserves = append(reserves, reserve)
		return r.request, 12000, nil
	}
	if _, err := r.run(t.Context()); err != nil || len(reserves) != 2 {
		t.Fatal("bounded transport contract round failed", err)
	}
	framing, err := structuredTransportReserve(nil, 3000)
	if err != nil || reserves[0] != framing+contextengine.Estimate(r.schema.contract) || reserves[1] <= reserves[0] {
		t.Fatal("private Registry contract/receipt budget was not reserved")
	}
}

func TestStructuredLayerTransportUnsupportedShapesFailClosed(t *testing.T) {
	for _, params := range []map[string]any{
		{"type": "object"},
		{"type": "object", "properties": map[string]any{"data": map[string]any{"type": "object", "additionalProperties": true}}},
		{"type": "object", "properties": map[string]any{"data": map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}}},
	} {
		_, err := newActionSchema([]llm.Tool{{Type: llm.ToolFunction, Name: "unsupported", Parameters: params}})
		requireCode(t, err, "bridge_schema_invalid")
	}
	_, err := newActionSchema([]llm.Tool{
		{Type: llm.ToolFunction, Name: "stringTool", Parameters: map[string]any{"type": "object", "properties": map[string]any{"same": map[string]any{"type": "string"}}}},
		{Type: llm.ToolFunction, Name: "integerTool", Parameters: map[string]any{"type": "object", "properties": map[string]any{"same": map[string]any{"type": "integer"}}}},
	})
	requireCode(t, err, "bridge_schema_invalid")
}

func TestStructuredLayerTransportRegistrySupersetAndStrictGuard(t *testing.T) {
	v := probeTransportVariant(t, 7)
	for _, tool := range layerRegistryTools(t) {
		raw, _ := json.Marshal(map[string]any{"type": "action", "action": map[string]any{"id": "fixture_1", "tool": tool.Name, "arguments": layerSchemaWitness(t, tool.Parameters)}})
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil || v.Schema.validator.Validate(value) != nil {
			t.Fatal("transport is not a Registry superset", tool.Name)
		}
		if d := validateStructuredProbeVariantOutput(v, raw); !d.accepted() || d.Outcome != probeValidatedAction {
			t.Fatal("valid Registry proposal rejected", tool.Name, d.summary())
		}
	}
	for _, invalid := range []string{
		`{"type":"final","final":{"message":"adapter schema probe OK"},"action":{"id":"a","tool":"read","arguments":{"path":"private-probe-path"}}}`,
		`{"type":"action","action":{"id":"bad/id","tool":"read","arguments":{"path":"private-probe-path"}}}`,
		`{"type":"action","action":{"id":"a","tool":"read","arguments":{"command":"private-probe-command"}}}`,
		`{"type":"action","action":{"id":"a","tool":"read","arguments":{"path":"private-probe-path","lines":0}}}`,
		`{"type":"other"}`, `{"type":"action"}`, `{}`, `{"type":"final"}`,
	} {
		d := runStructuredProbeVariantFixture(t, v, structuredProbeStream(invalid))
		if d.accepted() {
			t.Fatal("strict semantic guard was bypassed")
		}
	}
}

func TestStructuredLayerTransportProbeFinalContentMismatch(t *testing.T) {
	v := probeTransportVariant(t, 3)
	for _, message := range []string{"adapter schema probe OK.", "private-probe-unrequested-message"} {
		raw, _ := json.Marshal(map[string]any{"type": "final", "final": map[string]any{"message": message}})
		d := runStructuredProbeVariantFixture(t, v, structuredProbeStream(string(raw)))
		if d.accepted() || d.Outcome != probeValidatedFinal || d.Reason != probeFinalMessageMismatch || !d.SchemaChecked || !d.SchemaValid || !d.StructuredOutputPresent {
			t.Fatal("valid schema plus wrong fixed content was misclassified", d.summary())
		}
	}
	if !strings.Contains(structuredProbeSystem, structuredProbeFinal) {
		t.Fatal("probe's exact response target is not an unambiguous JSON literal")
	}
}

func TestStructuredOfficialControlTransportVariants(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	for level := 1; level <= 7; level++ {
		v := probeTransportVariant(t, level)
		t.Run(v.Name, func(t *testing.T) {
			before := len(f.Calls(t))
			report := runStructuredProbeVariant(t.Context(), v, model, func(ctx context.Context, invocation structuredProbeInvocation, v probeSchemaVariant) structuredProbeDiagnostic {
				return structuredProbeVariantGeneration(ctx, path, env, invocation, v)
			})
			if !report.Diagnostic.accepted() || report.Diagnostic.Outcome != probeValidatedFinal || len(f.Calls(t)) != before+1 || len(f.StructuredProbes(t)) != 0 || len(f.CatalogCalls(t)) != 0 {
				t.Fatal("fake transport generation failed or invoked execution/MCP", report.Diagnostic.summary())
			}
			assertProbeVariantValueFree(t, report.Diagnostic)
			call := f.Calls(t)[before]
			want, err := buildStructuredRequest(model, env, v.Schema, call.Directory+"/system.txt", structuredProbeInput, false)
			if err != nil || call.Initialize != string(want.Initialize) || call.Input != structuredProbeInput || call.System != structuredProbeSystem || call.ChildPID != 0 {
				t.Fatal("fake transport request changed")
			}
			if strings.Contains(call.Schema, `"oneOf"`) {
				t.Fatal("oneOf reached provider transport")
			}
		})
	}
}
