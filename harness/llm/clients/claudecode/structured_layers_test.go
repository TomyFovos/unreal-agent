//go:build linux || darwin

package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	subagenttool "github.com/unreallabsai/unreal-agent/harness/tool/subagent"
)

var updateStructuredFixtures = flag.Bool("update-structured-fixtures", false, "update synthetic structured schema/request snapshots (no provider)")

func layerRegistryTools(t *testing.T) []llm.Tool {
	t.Helper()
	registry := tool.NewRegistry(tool.StaticTranslators{}, tool.StaticNames()...)
	var tools []llm.Tool
	for _, def := range registry.StaticDefinitions() {
		tools = append(tools, def.Tool)
	}
	extensions, err := subagenttool.Extensions("synthetic-owner", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range extensions {
		tools = append(tools, extension.Definition.Tool)
	}
	return tools
}

func structuredGolden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("testdata", "structured", name)
	if *updateStructuredFixtures {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(encoded, want) {
		t.Fatal("synthetic schema/request snapshot mismatch", name, err)
	}
}

// Construct a schema witness only for unit tests. This is never used to repair,
// infer or execute provider arguments. Each witness must pass the real validator.
func layerSchemaWitness(t *testing.T, raw any) any {
	t.Helper()
	encoded, _ := json.Marshal(raw)
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	if v, ok := schema["const"]; ok {
		return v
	}
	if values, ok := schema["enum"].([]any); ok {
		return values[0]
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if branches, ok := schema[key].([]any); ok {
			return layerSchemaWitness(t, branches[0])
		}
	}
	switch schema["type"] {
	case "object":
		value := map[string]any{}
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, name := range required {
			key := name.(string)
			value[key] = layerSchemaWitness(t, properties[key])
		}
		return value
	case "array":
		value := []any{}
		minimum, _ := schema["minItems"].(float64)
		for i := 0; i < int(minimum); i++ {
			value = append(value, layerSchemaWitness(t, schema["items"]))
		}
		return value
	case "boolean":
		return false
	case "integer", "number":
		if minimum, ok := schema["minimum"]; ok {
			return minimum
		}
		return 1
	case "string":
		return "synthetic"
	default:
		t.Fatalf("schema witness type unsupported: %v", schema["type"])
		return nil
	}
}

func TestStructuredLayerSchemaRegistryBranches(t *testing.T) {
	tools := layerRegistryTools(t)
	schema, err := newActionSchema(tools)
	if err != nil || schema.validator.DraftVersion != 7 {
		t.Fatal("Draft 7 compile", err)
	}
	structuredGolden(t, "registry.schema.json", schema.document)
	for i, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			proposal := map[string]any{"type": "action", "action": map[string]any{"id": "synthetic_1", "tool": tool.Name, "arguments": layerSchemaWitness(t, tool.Parameters)}}
			encoded, _ := json.Marshal(proposal)
			parsed, err := schema.parse(encoded)
			if err != nil || parsed.Action == nil || parsed.Action.Tool != tool.Name {
				t.Fatal("registry branch rejected", err)
			}
			value, _ := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
			matches := 0
			for _, branch := range schema.validator.OneOf {
				if branch.Validate(value) == nil {
					matches++
				}
			}
			if matches != 1 || schema.validator.OneOf[i+1].Validate(value) != nil {
				t.Fatal("oneOf branch ambiguity")
			}
			proposal["unexpected"] = true
			encoded, _ = json.Marshal(proposal)
			if _, err := schema.parse(encoded); err == nil {
				t.Fatal("additional root property accepted")
			}
			delete(proposal, "unexpected")
			proposal["action"].(map[string]any)["id"] = "invalid/id"
			encoded, _ = json.Marshal(proposal)
			requireStructuredStage(t, schemaError(schema, encoded), "structured_schema_rejected")
		})
	}
	for _, raw := range []string{
		`{"type":"final","final":{"message":"public"}}`,
		`{"type":"final","final":{"message":"日本語 👩🏽‍💻\n\u0060\u0060\u0060code\u0060\u0060\u0060"}}`,
	} {
		if _, err := schema.parse(jsontext.Value(raw)); err != nil {
			t.Fatal("Final branch", err)
		}
	}
}

func schemaError(schema *actionSchema, raw jsontext.Value) error {
	_, err := schema.parse(raw)
	return err
}

func TestStructuredLayerSchemaRejectsUntrustedDefinitions(t *testing.T) {
	for _, parameters := range []map[string]any{
		{"type": "not-a-json-schema-type"},
		{"type": "object", "$ref": "https://unreal.invalid/unknown.json"},
		{"type": "object", "$ref": "file:///unreadable-schema"},
		{"type": "object", "description": strings.Repeat("x", structuredResponseBytes)},
	} {
		_, err := newActionSchema([]llm.Tool{{Type: llm.ToolFunction, Name: "synthetic", Parameters: parameters}})
		requireCode(t, err, "bridge_schema_invalid")
	}
	for _, raw := range []string{`{"type":"final"}`, `{"type":"final","final":{}}`, `{"type":"final","final":{"message":"x"},"action":{}}`, `{"type":"action","action":{"id":"","tool":"read","arguments":{"path":"x"}}}`} {
		if _, err := diagnosticSchema(t).parse(jsontext.Value(raw)); err == nil {
			t.Fatal("required/union/id check accepted invalid proposal")
		}
	}
}

func TestStructuredLayerRequestSnapshot(t *testing.T) {
	c := &Client{config: Config{ManagedPolicyMode: ManagedPolicyTrust, Getenv: func(key string) string {
		if key == "HOME" {
			return "/synthetic-home"
		}
		// Values are innocuous markers, never fixture credentials. The builder
		// must not inherit any of these excluded namespaces.
		if key == "ANTHROPIC_API_KEY" || key == "OPENAI_API_KEY" || key == "GH_TOKEN" || strings.HasPrefix(key, "OTEL_") {
			return "fixture-excluded"
		}
		return ""
	}}}
	env, err := c.environment()
	if err != nil {
		t.Fatal(err)
	}
	model := llm.Model{ID: "synthetic-model", ReasoningEffort: llm.ReasoningEffortHigh}
	request, err := buildStructuredRequest(model, env, diagnosticSchema(t), "/private-temp/system.txt", "public task\n日本語", false)
	if err != nil {
		t.Fatal(err)
	}
	structuredGolden(t, "request.json", request)
	if err := validateStructuredLaunch(request.Arguments); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{"Bash", "Read", "Edit", "Agent", "mcp__*"} {
		if !slices.Contains(strings.Split(structuredDeny, ","), denied) {
			t.Fatal("missing execution deny")
		}
	}
	for _, entry := range request.Environment {
		if strings.Contains(entry, "fixture-excluded") {
			t.Fatal("excluded environment inherited")
		}
	}
	if strings.Contains(strings.Join(request.Arguments, " "), "public task") {
		t.Fatal("prompt in argv")
	}
	probe, err := buildStructuredRequest(llm.Model{}, env, diagnosticSchema(t), "/private-temp/system.txt", "unused", true)
	if err != nil || probe.Input != nil || slices.Contains(probe.Arguments, "-p") {
		t.Fatal("capability probe would infer", err)
	}
	model.ReasoningEffort = ""
	plain, err := buildStructuredRequest(model, env, diagnosticSchema(t), "/private-temp/system.txt", "task", false)
	if err != nil || slices.Contains(plain.Arguments, "--effort") {
		t.Fatal("unsupported effort was added", err)
	}
	for _, flag := range []string{"--safe-mode", "--tools", "--allowedTools", "--strict-mcp-config", "--mcp-config"} {
		args := append([]string(nil), request.Arguments...)
		i := slices.Index(args, flag)
		args = append(args[:i], args[i+1:]...)
		requireCode(t, validateStructuredLaunch(args), "isolation_contract_invalid")
	}
	request.Environment[0] = "changed"
	if env[0] == "changed" {
		t.Fatal("request mutated environment snapshot")
	}
}

func TestStructuredLayerProtocolFrames(t *testing.T) {
	schema := diagnosticSchema(t)
	for _, probe := range []bool{true, false} {
		init, input, err := structuredProtocolFrames(schema, "public task\nsecond line", probe)
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Request struct {
				Hooks   map[string]any `json:"hooks"`
				Servers []string       `json:"sdkMcpServers"`
				Schema  jsontext.Value `json:"jsonSchema"`
			} `json:"request"`
		}
		if json.Unmarshal(init, &frame) != nil || frame.Request.Hooks == nil || frame.Request.Servers == nil || len(frame.Request.Servers) != 0 || !bytes.Equal(frame.Request.Schema, schema.requestDocument()) {
			t.Fatal("control contract changed")
		}
		if (input == nil) != probe {
			t.Fatal("user frame emitted during probe")
		}
	}
	_, _, err := structuredProtocolFrames(nil, "task", false)
	requireCode(t, err, "bridge_schema_invalid")
}

func TestStructuredLayerPromptFramingAndPrivateExclusion(t *testing.T) {
	items := []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "PROJECT_SNAPSHOT"}},
		{Type: llm.ItemMessage, ProviderID: "private-helper-id", Data: llm.Message{Role: llm.RoleUser, Text: "public task\n日本語"}},
		{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: jsontext.Value(`{"hidden":"private-sensitive"}`)}},
	}
	last := llm.ToolOutcome{Failed: true, Result: llm.ToolResult{CallID: "receipt", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "canonical failure feedback"}}}}
	items = append(items, llm.Item{Type: llm.ItemToolResult, Data: last.Result})
	system, input, err := structuredPrompt(items, &last, 1024)
	if err != nil || !strings.Contains(system, "PROJECT_SNAPSHOT") || !strings.Contains(system, structuredInstruction) || strings.Count(input, "canonical failure feedback") != 1 || !strings.Contains(input, `"Failed":true`) {
		t.Fatal("system/prompt/receipt framing", err)
	}
	if strings.Contains(system+input, "private-sensitive") || strings.Contains(system+input, "private-helper-id") || strings.Contains(input, structuredInstruction) {
		t.Fatal("private state/instruction became public conversation")
	}
	for _, receipt := range []llm.ToolOutcome{
		{Result: llm.ToolResult{Output: []llm.ToolResultOutput{{Kind: llm.ToolResultImage, Value: "dummy"}}}},
		{Result: llm.ToolResult{Output: make([]llm.ToolResultOutput, 129)}},
	} {
		_, _, err := structuredPrompt(items, &receipt, 1024)
		requireCode(t, err, "bridge_receipt_invalid")
	}
	last.Result.Output[0].Value = strings.Repeat("large receipt\n", 1000)
	_, input, err = structuredPrompt(items[:2], &last, 1024)
	if err != nil || !strings.Contains(input, "truncated") || len(input) > 4096 {
		t.Fatal("unbounded/silent result truncation", err)
	}
}
