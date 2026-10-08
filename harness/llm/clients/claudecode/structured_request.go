package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const structuredInitID = "unreal_structured_init"

// Claude Code 2.1.285 retries its serialization-only StructuredOutput validator
// up to five times. One CLI turn would prevent correction of its first invalid
// helper input. This is an internal serializer allowance, not Unreal actions:
// only the final validated result can reach the host, once per generation.
const structuredSerializerTurns = "5"

// A disposable request projection. No process, credentials, history or tool
// execution is needed to construct it. Environment is the already restricted
// snapshot returned by Client.environment/prepare, never the parent environment.
type structuredRequest struct {
	Arguments   []string
	Environment []string
	Initialize  jsontext.Value
	Input       jsontext.Value
}

func buildStructuredRequest(model llm.Model, environment []string, schema *actionSchema, systemPath, input string, probe bool) (structuredRequest, error) {
	args := append(structuredArgs(), "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--no-session-persistence", "--permission-prompts", "none", "--max-turns", structuredSerializerTurns, "--system-prompt-file", systemPath)
	if !probe {
		args = append(args, "-p", "--model", model.ID)
		if model.ReasoningEffort != "" {
			args = append(args, "--effort", string(model.ReasoningEffort))
		}
	}
	if err := validateStructuredLaunch(args); err != nil {
		return structuredRequest{}, err
	}
	initialize, user, err := structuredProtocolFrames(schema, input, probe)
	if err != nil {
		return structuredRequest{}, err
	}
	return structuredRequest{Arguments: args, Environment: append([]string(nil), environment...), Initialize: initialize, Input: user}, nil
}

// Both the request snapshot and the live control transport use these frames.
// A capability probe has no user frame and therefore cannot request inference.
func structuredProtocolFrames(schema *actionSchema, input string, probe bool) (jsontext.Value, jsontext.Value, error) {
	if schema == nil {
		return nil, nil, structuredError("bridge_schema_invalid", structuredInitFrameInvalid)
	}
	initialize, err := json.Marshal(map[string]any{"type": "control_request", "request_id": structuredInitID, "request": map[string]any{"subtype": "initialize", "hooks": map[string]any{}, "sdkMcpServers": []string{}, "jsonSchema": schema.requestDocument()}}, json.Deterministic(true))
	if err != nil {
		return nil, nil, structuredError("bridge_schema_invalid", structuredInitFrameInvalid)
	}
	if probe {
		return initialize, nil, nil
	}
	user, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": input}}, json.Deterministic(true))
	if err != nil {
		return nil, nil, structuredError("structured_protocol_invalid", structuredUnexpectedPreinitFrame)
	}
	return initialize, user, nil
}
