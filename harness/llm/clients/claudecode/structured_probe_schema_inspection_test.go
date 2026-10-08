//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Probe-only variants shared by the offline audit and explicit adapter probe.
// A/F use the existing level builders verbatim; B-E alter disposable copies of
// their object nodes to isolate each new structural element. Never used for
// production action validation or execution.
type probeSchemaVariant struct {
	Name   string
	Schema *actionSchema
	Strict *actionSchema // T7 additionally checks the unchanged execution contract.
}

func probeSchemaObject(t *testing.T, raw jsontext.Value) map[string]any {
	t.Helper()
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil || object == nil {
		t.Fatal("probe schema object malformed")
	}
	return object
}

func cloneProbeSchemaObject(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		t.Fatal("probe schema clone failed")
	}
	return probeSchemaObject(t, raw)
}

func probeSchemaVariants(t *testing.T) []probeSchemaVariant {
	t.Helper()
	final, generic := structuredProbeSchema(t, 1), structuredProbeSchema(t, 2)
	finalObject := probeSchemaObject(t, final.document)
	genericObject := probeSchemaObject(t, generic.document)
	actionBranch := genericObject["oneOf"].([]any)[1].(map[string]any)
	fullAction := actionBranch["properties"].(map[string]any)["action"].(map[string]any)
	fullProperties := fullAction["properties"].(map[string]any)
	minimalAction := map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "additionalProperties": false}
	b := cloneProbeSchemaObject(t, finalObject)
	b["properties"].(map[string]any)["action"] = cloneProbeSchemaObject(t, minimalAction)
	cBranch := cloneProbeSchemaObject(t, actionBranch)
	cBranch["properties"].(map[string]any)["action"] = cloneProbeSchemaObject(t, minimalAction)
	c := map[string]any{"type": "object", "oneOf": []any{cloneProbeSchemaObject(t, finalObject), cBranch}}
	d := cloneProbeSchemaObject(t, c)
	properties := func(root map[string]any) map[string]any {
		return root["oneOf"].([]any)[1].(map[string]any)["properties"].(map[string]any)["action"].(map[string]any)["properties"].(map[string]any)
	}
	properties(d)["tool"] = cloneProbeSchemaObject(t, fullProperties["tool"].(map[string]any))
	e := cloneProbeSchemaObject(t, d)
	properties(e)["arguments"] = cloneProbeSchemaObject(t, fullProperties["arguments"].(map[string]any))
	variants := []probeSchemaVariant{{Name: "A", Schema: final}}
	for i, object := range []map[string]any{b, c, d, e} {
		document, err := json.Marshal(object, json.Deterministic(true))
		if err != nil {
			t.Fatal("probe variant encoding failed")
		}
		validator, err := compileStructuredDocument(document)
		if err != nil {
			t.Fatal("probe variant compile failed")
		}
		variants = append(variants, probeSchemaVariant{Name: string(rune('B' + i)), Schema: &actionSchema{document: document, validator: validator, tools: map[string]bool{}}})
	}
	return append(variants, probeSchemaVariant{Name: "F", Schema: generic})
}

func selectedProbeSchemaVariant(t *testing.T, name string) probeSchemaVariant {
	t.Helper()
	if isTransportProbeVariant(name) {
		return probeTransportVariant(t, int(name[1]-'0'))
	}
	if name == "C1" || name == "C2" {
		for _, variant := range probeMicroSchemaVariants(t) {
			if variant.Name == name {
				return variant
			}
		}
	}
	for _, variant := range probeSchemaVariants(t) {
		if variant.Name == name {
			return variant
		}
	}
	t.Fatal("invalid schema variant selection")
	return probeSchemaVariant{}
}

// C1/C2 isolate oneOf presence and branch count without changing any A-F bytes.
// Retain the already-tested root object type to avoid conflating an absent
// root type with the new union. The exact Level 1 bytes form the Final branch.
func probeMicroSchemaVariants(t *testing.T) []probeSchemaVariant {
	t.Helper()
	variants := probeSchemaVariants(t)
	final := variants[0].Schema
	c := probeSchemaObject(t, variants[2].Schema.document)
	minimalTag := cloneProbeSchemaObject(t, c["oneOf"].([]any)[1].(map[string]any))
	delete(minimalTag["properties"].(map[string]any), "action")
	minimalTag["required"] = []string{"type"}
	var micro []probeSchemaVariant
	for i, branches := range [][]any{{jsontext.Value(final.document)}, {jsontext.Value(final.document), minimalTag}} {
		document, err := json.Marshal(map[string]any{"type": "object", "oneOf": branches}, json.Deterministic(true))
		if err != nil {
			t.Fatal("micro variant encoding failed")
		}
		validator, err := compileStructuredDocument(document)
		if err != nil {
			t.Fatal("micro variant Draft 7 compile failed")
		}
		micro = append(micro, probeSchemaVariant{Name: fmt.Sprintf("C%d", i+1), Schema: &actionSchema{document: document, validator: validator, tools: map[string]bool{}}})
	}
	return micro
}

type probeSchemaInspection struct {
	Keywords                                  map[string]int
	Nodes, Objects, Properties, MaxProperties int
	Depth, ObjectsWithoutProperties           int
	ObjectNesting                             int
	StrictUnsupported                         []string
}

// This is keyword/shape inspection, NOT a provider validator or normalizer.
// The strict keyword vocabulary is frozen from installed 2.1.285 byte 201237013
// (qe/Njn/w). Unsupported strict derivation is not provider rejection: Ze/FI can
// retain the original non-strict schema. No runtime feature flag is assumed.
func inspectProbeSchema(t *testing.T, document jsontext.Value) probeSchemaInspection {
	t.Helper()
	inspection := probeSchemaInspection{Keywords: map[string]int{}}
	strictKeywords := []string{"$schema", "type", "description", "title", "properties", "required", "additionalProperties", "items", "enum", "const", "anyOf"}
	var walk func(map[string]any, int, int)
	walk = func(node map[string]any, depth, parentObjects int) {
		inspection.Nodes++
		inspection.Depth = max(inspection.Depth, depth)
		for keyword := range node {
			inspection.Keywords[keyword]++
			if !slices.Contains(strictKeywords, keyword) {
				inspection.StrictUnsupported = append(inspection.StrictUnsupported, keyword)
			}
		}
		properties, hasProperties := node["properties"].(map[string]any)
		if node["type"] == "object" {
			inspection.Objects++
			inspection.ObjectNesting = max(inspection.ObjectNesting, parentObjects+1)
			if !hasProperties {
				inspection.ObjectsWithoutProperties++
			}
		}
		inspection.Properties += len(properties)
		inspection.MaxProperties = max(inspection.MaxProperties, len(properties))
		for _, name := range slices.Sorted(maps.Keys(properties)) {
			walk(properties[name].(map[string]any), depth+1, parentObjects+1)
		}
		for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
			branches, _ := node[keyword].([]any)
			for _, branch := range branches {
				// Union branches constrain the same value; they do not add an
				// object layer to the provider's output instance.
				walk(branch.(map[string]any), depth+1, parentObjects)
			}
		}
	}
	walk(probeSchemaObject(t, document), 1, 0)
	slices.Sort(inspection.StrictUnsupported)
	inspection.StrictUnsupported = slices.Compact(inspection.StrictUnsupported)
	return inspection
}

func TestStructuredLayerProbeLevelOneTwoExactSchemaDiff(t *testing.T) {
	one, two := structuredProbeSchema(t, 1), structuredProbeSchema(t, 2)
	a, b := inspectProbeSchema(t, one.document), inspectProbeSchema(t, two.document)
	var added []string
	for keyword := range b.Keywords {
		if a.Keywords[keyword] == 0 {
			added = append(added, keyword)
		}
	}
	slices.Sort(added)
	if !slices.Equal(added, []string{"maxLength", "minLength", "oneOf", "pattern"}) ||
		a.Nodes != 4 || a.Objects != 2 || a.Properties != 3 || a.MaxProperties != 2 || a.Depth != 3 || a.ObjectNesting != 2 ||
		b.Nodes != 11 || b.Objects != 6 || b.Properties != 8 || b.MaxProperties != 3 || b.Depth != 4 || b.ObjectNesting != 3 || b.ObjectsWithoutProperties != 2 {
		t.Fatal("exact schema keyword/shape difference changed")
	}
	for _, keyword := range []string{"anyOf", "allOf", "enum", "$ref", "$defs", "definitions"} {
		if a.Keywords[keyword] != 0 || b.Keywords[keyword] != 0 {
			t.Fatal("schema diff unexpectedly includes another union, reference or enum")
		}
	}
	root := probeSchemaObject(t, two.document)
	if !slices.Equal(slices.Sorted(maps.Keys(root)), []string{"oneOf", "type"}) {
		t.Fatal("Level 2 root shape changed")
	}
	branches := root["oneOf"].([]any)
	if !reflect.DeepEqual(branches[0], probeSchemaObject(t, one.document)) {
		t.Fatal("Final branch itself changed")
	}
	props := branches[1].(map[string]any)["properties"].(map[string]any)["action"].(map[string]any)["properties"].(map[string]any)
	if !reflect.DeepEqual(props["tool"], map[string]any{"type": "string"}) ||
		!reflect.DeepEqual(props["arguments"], map[string]any{"type": "object", "additionalProperties": false}) {
		t.Fatal("generic tool/arguments contract changed")
	}
	if a.Keywords["const"] != 1 || b.Keywords["const"] != 2 || a.Keywords["required"] != 2 || b.Keywords["required"] != 4 ||
		a.Keywords["additionalProperties"] != 2 || b.Keywords["additionalProperties"] != 5 {
		t.Fatal("discriminator, required or closed-object structure changed")
	}
	t.Log("Level 1 -> 2: new oneOf/minLength/maxLength/pattern; root properties/required/additionalProperties moved into branches; generic tool has no enum/const; arguments is a closed object without properties; no references or anyOf/allOf")
}

func TestStructuredLayerProbeOfflineSchemaVariants(t *testing.T) {
	want := []struct {
		bytes       int
		hash        string
		unsupported []string
	}{
		{235, "c703d71094a2f7c5555ae6d194a12293917421e44e6f4e930ffd86e9b1d9cfd9", nil},
		{329, "a640f9b702ffed27b4be45e97e8f357c5d906d8809f8f7a4d9569da6b4382e82", nil},
		{474, "31818fd70e7f563717a8e857b36803dbf0b4e6d0ff4e4607bd93501b147b870a", []string{"oneOf"}},
		{499, "7b5530638af483c115a4a65cabacdb453145ddeb2216ae5cb00b4e5d55c5f20e", []string{"oneOf"}},
		{558, "6cdbe448806add436aa822a7b98a7a2eeb06708bf1a201df3828ecc6e77c5601", []string{"oneOf"}},
		{653, "bcf0ceb61ffeb20ccdd18b854fb5152583ea51e16ebd0d8a778cee38941e2596", []string{"maxLength", "minLength", "oneOf", "pattern"}},
	}
	for i, variant := range probeSchemaVariants(t) {
		t.Run(variant.Name, func(t *testing.T) {
			schema := variant.Schema
			hash := fmt.Sprintf("%x", sha256.Sum256(schema.document))
			inspection := inspectProbeSchema(t, schema.document)
			if len(schema.document) != want[i].bytes || hash != want[i].hash || !slices.Equal(inspection.StrictUnsupported, want[i].unsupported) || schema.validator.DraftVersion != 7 {
				t.Fatal("offline variant byte/hash/Draft/strict-keyword snapshot changed")
			}
			p, err := schema.parse(jsontext.Value(structuredProbeFinal))
			if err != nil || p.Type != "final" || p.Final == nil || p.Action != nil {
				t.Fatal("variant rejected the fixed Final")
			}
			for _, invalid := range []string{`{"type":"final","final":{"message":"OK"},"extra":true}`, `{"type":"final"}`, `{"type":"action","action":{},"final":{"message":"OK"}}`, `{"type":"final","final":{"message":"OK"},"action":{}}`} {
				if _, err := schema.parse(jsontext.Value(invalid)); err == nil {
					t.Fatal("intermediate schema bypassed authoritative envelope validation")
				}
			}
			t.Logf("offline variant=%s schema_bytes=%d schema_sha256=%s strict_unsupported=%v", variant.Name, len(schema.document), hash, inspection.StrictUnsupported)
		})
	}
}

func TestStructuredLayerProbeVariantRequestsChangeOnlySchema(t *testing.T) {
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	var baseline probeRequestSnapshot
	for i, variant := range probeSchemaVariants(t) {
		invocation := newStructuredProbeInvocation(model, variant.Schema)
		request, err := buildStructuredRequest(model, []string{"HOME=/synthetic-home", "PATH=/usr/bin"}, variant.Schema, "/private-temp/system.txt", invocation.Input, false)
		if err != nil {
			t.Fatal("offline request construction failed")
		}
		var init map[string]jsontext.Value
		if json.Unmarshal(request.Initialize, &init) != nil {
			t.Fatal("initialize decode failed")
		}
		var payload map[string]jsontext.Value
		if json.Unmarshal(init["request"], &payload) != nil || !bytes.Equal(payload["jsonSchema"], variant.Schema.document) {
			t.Fatal("provider request schema differs from inspected in-memory bytes")
		}
		delete(payload, "jsonSchema")
		init["request"], _ = json.Marshal(payload, json.Deterministic(true))
		request.Initialize, _ = json.Marshal(init, json.Deterministic(true))
		snapshot := probeRequestSnapshot{Model: invocation.Model, System: invocation.System, Input: invocation.Input, Timeout: invocation.Timeout, Request: request}
		if i == 0 {
			baseline = snapshot
		} else if !reflect.DeepEqual(snapshot, baseline) {
			t.Fatal("variant leaked into prompt, system, argv/env, runtime, timeout, tool restrictions or session settings")
		}
	}
}

func TestStructuredOfficialControlProbeOfflineSchemaVariants(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range probeSchemaVariants(t) {
		invocation := newStructuredProbeInvocation(llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}, variant.Schema)
		ctx, cancel := context.WithTimeout(t.Context(), invocation.Timeout)
		p, response, err := c.structuredGeneration(ctx, path, env, invocation.Model, invocation.Schema, invocation.System, invocation.Input, false)
		cancel()
		d := classifyStructuredProbe(p, response, err)
		if !d.accepted() || d.Outcome != probeValidatedFinal || len(response.Output) != 0 {
			t.Fatal("fake adapter variant failed", variant.Name, d.summary())
		}
		calls := f.Calls(t)
		call := calls[len(calls)-1]
		want, err := buildStructuredRequest(invocation.Model, env, variant.Schema, call.Directory+"/system.txt", invocation.Input, false)
		if err != nil || call.Initialize != string(want.Initialize) || call.System != invocation.System || call.UserFrame != string(want.Input) ||
			!slices.Equal(call.Arguments, want.Arguments) || !slices.Equal(call.Environment, want.Environment) {
			t.Fatal("fake CLI received a different schema/request")
		}
	}
	if len(f.Calls(t)) != 6 || len(f.CatalogCalls(t)) != 0 || len(f.StructuredProbes(t)) != 0 {
		t.Fatal("variant unexpectedly invoked catalog/MCP or reused a generation")
	}
	seen := map[int]bool{}
	for _, call := range f.Calls(t) {
		if call.PID <= 0 || seen[call.PID] || strings.Contains(call.Input, "variant=") {
			t.Fatal("variant reused subprocess/state or leaked its selector into input")
		}
		seen[call.PID] = true
	}
	t.Log("offline A-F: same request except schema; 6 fresh fake generations; Final only; execution/MCP/Operations=0")
}
