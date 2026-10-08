package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const structuredOutputTool = "StructuredOutput"
const structuredResponseBytes = 1 << 20

type actionProposal struct {
	Type   string `json:"type"`
	Action *struct {
		ID        string         `json:"id"`
		Tool      string         `json:"tool"`
		Arguments jsontext.Value `json:"arguments"`
	} `json:"action,omitempty"`
	Final *struct {
		Message string `json:"message"`
	} `json:"final,omitempty"`
}

type actionSchema struct {
	document  jsontext.Value
	transport jsontext.Value
	contract  string
	validator *jsonschema.Schema
	tools     map[string]bool
	arguments map[string]*jsonschema.Schema
}

// All schemas come from the Host registry. Schema compilation has no file or
// network loader: even a malicious $ref cannot acquire execution authority.
type noSchemaLoader struct{}

func (noSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("external schema resources disabled")
}

func newActionSchema(tools []llm.Tool) (*actionSchema, error) {
	registered, err := bridgeTools(tools)
	if err != nil {
		return nil, err
	}
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	final := object(map[string]any{
		"type":  map[string]any{"const": "final"},
		"final": object(map[string]any{"message": map[string]any{"type": "string", "maxLength": structuredResponseBytes}}, "message"),
	}, "type", "final")
	branches := []any{final}
	names := map[string]bool{}
	for _, t := range registered {
		if t.original == structuredOutputTool {
			return nil, &Error{Code: "bridge_schema_invalid"}
		}
		names[t.original] = true
		branches = append(branches, object(map[string]any{
			"type": map[string]any{"const": "action"},
			"action": object(map[string]any{
				"id":        map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": "^[A-Za-z0-9_-]+$"},
				"tool":      map[string]any{"const": t.original, "description": t.Description},
				"arguments": t.Schema,
			}, "id", "tool", "arguments"),
		}, "type", "action"))
	}
	// This exclusive union is authoritative ONLY in Unreal. Real 2.1.285 Opus
	// rejected a single root oneOf before StructuredOutput. Keep every semantic
	// constraint here, and give the serializer a separately compiled closed
	// transport object. Transport acceptance never authorizes an Action.
	document := map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "type": "object", "oneOf": branches}
	data, err := json.Marshal(document, json.Deterministic(true))
	if err != nil || len(data) > structuredResponseBytes {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	validator, err := compileStructuredDocument(data)
	if err != nil {
		return nil, err
	}
	// Both projections derive from this immutable snapshot, never mutable caller
	// maps. Only the strict validator can yield a proposal for the Host.
	arguments := make(map[string]*jsonschema.Schema, len(registered))
	for i, t := range registered {
		arguments[t.original] = validator.OneOf[i+1].Properties["action"].Properties["arguments"]
	}
	transport, err := structuredTransportDocument(data)
	if err != nil {
		return nil, err
	}
	contract, err := structuredRegistryContract(data)
	if err != nil {
		return nil, err
	}
	return &actionSchema{document: data, transport: transport, contract: contract, validator: validator, tools: names, arguments: arguments}, nil
}

func (s *actionSchema) requestDocument() jsontext.Value {
	if len(s.transport) != 0 {
		return s.transport
	}
	return s.document // Only standalone schema fixtures/probes have one projection.
}

func compileStructuredDocument(data jsontext.Value) (*jsonschema.Schema, error) {
	if len(data) > structuredResponseBytes || validateStructuredJSON(data) != nil {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	compiler.UseLoader(noSchemaLoader{})
	const location = "https://unreal.invalid/action-schema.json"
	if err = compiler.AddResource(location, doc); err != nil {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	validator, err := compiler.Compile(location)
	if err != nil {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	return validator, nil
}

func (s *actionSchema) parse(raw jsontext.Value) (actionProposal, error) {
	var proposal actionProposal
	detail := StructuredDiagnostics{StructuredOutputPresent: len(raw) != 0, StructuredOutputNull: raw.Kind() == 'n', StructuredOutputBytes: len(raw)}
	failure := func(code string, stage structuredStage) error {
		return annotateStructured(structuredError(code, stage), stage, nil, detail)
	}
	if len(raw) == 0 {
		return proposal, failure("structured_protocol_invalid", structuredResultMissingOutput)
	}
	if raw.Kind() == 'n' {
		return proposal, failure("structured_protocol_invalid", structuredResultNullOutput)
	}
	if len(raw) > structuredResponseBytes {
		return proposal, failure("structured_response_too_large", structuredResponseOversized)
	}
	if err := validateStructuredJSON(raw); err != nil {
		return actionProposal{}, annotateStructured(err, structuredJSONDecodeFailed, nil, detail)
	}
	// The public contract is result.structured_output, not assistant helper
	// input. Decode and validate that value locally before any Host ToolCall.
	if json.Unmarshal(raw, &proposal, json.RejectUnknownMembers(true)) != nil {
		return actionProposal{}, failure("structured_protocol_invalid", structuredActionEnvelopeInvalid)
	}
	if proposal.Type == "action" && proposal.Action != nil && proposal.Final == nil {
		if !s.tools[proposal.Action.Tool] {
			return actionProposal{}, failure("bridge_unknown_tool", structuredUnknownTool)
		}
		args := proposal.Action.Arguments
		detail.SchemaChecked = true
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
		validator := s.arguments[proposal.Action.Tool]
		if args.Kind() != '{' || err != nil || validator == nil || validator.Validate(value) != nil {
			return actionProposal{}, failure("bridge_arguments_invalid", structuredArgumentsSchemaRejected)
		}
		// Legacy Registry schemas such as Bash omit additionalProperties. Even
		// there, a different tool's field from the transport superset is not an
		// argument to this tool. Reject it before producing any ToolCall.
		for name := range value.(map[string]any) {
			if _, declared := validator.Properties[name]; !declared {
				return actionProposal{}, failure("bridge_arguments_invalid", structuredArgumentsSchemaRejected)
			}
		}
	} else if proposal.Type != "final" || proposal.Final == nil || proposal.Action != nil {
		return actionProposal{}, failure("structured_protocol_invalid", structuredActionEnvelopeInvalid)
	}
	detail.SchemaChecked = true
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil || s.validator.Validate(value) != nil {
		return actionProposal{}, failure("structured_protocol_invalid", structuredSchemaRejected)
	}
	return proposal, nil
}

func actionFingerprint(p actionProposal) (string, error) {
	// Preserve JSON numbers, including integers beyond float64 precision.
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(p.Action.Arguments))
	if err != nil {
		return "", structuredError("bridge_arguments_invalid", structuredArgumentsSchemaRejected)
	}
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", structuredError("bridge_arguments_invalid", structuredArgumentsSchemaRejected)
	}
	return p.Action.Tool + "\n" + string(data), nil
}
