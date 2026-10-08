package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// This serializer exception is private to structured mode. It cannot return a
// host ToolCall: only result.structured_output, independently schema-validated,
// becomes an Action/Final. All other execution is rejected by the same isolated
// stream validator as text-only mode. No raw JSON/prose reaches progress/UI.
func readStructuredStream(scanner *bufio.Scanner, schema *actionSchema, prefix []byte, finish func()) (actionProposal, llm.Response, error) {
	var clean bytes.Buffer
	clean.Write(prefix)
	serializerIDs := map[string]bool{}
	serializerBlocks := map[int]int{}
	activeBlocks := map[int]bool{}
	var diagnostic StructuredDiagnostics
	systems := structuredSystemValidator{}
	// The control initialization can precede the generation. Retain just its
	// validated init frame, not all earlier metadata, for per-frame checks.
	for _, line := range bytes.Split(prefix, []byte{'\n'}) {
		var v streamRecord
		if json.Unmarshal(line, &v) == nil && v.Type == "system" && v.Subtype == "init" {
			systems.init = append(append([]byte(nil), line...), '\n')
		}
	}
	fail := func(err error, stage structuredStage, line []byte) (actionProposal, llm.Response, error) {
		diagnostic.StructuredOutputBlocks = len(serializerIDs)
		return actionProposal{}, llm.Response{}, annotateStructured(err, stage, line, diagnostic)
	}
	var proposal actionProposal
	var response llm.Response
	finished := false
	total, count := len(prefix), 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line)
		count++
		if total > 32<<20 || count > 65536 {
			return fail(structuredError("structured_response_too_large", structuredResponseOversized), structuredResponseOversized, line)
		}
		if err := validateStructuredJSON(line); err != nil {
			return fail(err, structuredStreamFrameInvalid, line)
		}
		// The pinned SDK transport filters this exact heartbeat both during a
		// generation and after its result. It conveys no execution or content.
		if isStructuredKeepAlive(line) {
			continue
		}
		if finished {
			return fail(structuredError("structured_protocol_invalid", structuredTrailingFrame), structuredTrailingFrame, line)
		}
		if structuredReminder(line) {
			continue // exact local serialization reminder, never a host input
		}
		if recognized, err := permissionDeniedEvent(line, false, nil); recognized {
			return fail(structuredDenial(err), structuredPermissionDenied, line)
		}
		var record streamRecord
		var header struct {
			Type string `json:"type"`
		}
		var decodeErr error
		if json.Unmarshal(line, &header) == nil && header.Type == "assistant" {
			record, decodeErr = structuredAssistant(line, &diagnostic)
		} else {
			decodeErr = json.Unmarshal(line, &record)
		}
		if decodeErr != nil {
			stage := structuredStreamFrameInvalid
			shape := structuredShape(line)
			switch shape.Type {
			case "system":
				stage = structuredSystemFrameInvalid
				if shape.Subtype == "init" {
					stage = structuredInitFrameInvalid
				}
			case "assistant":
				stage = structuredSerializerFrameInvalid
			case "stream_event":
				stage = structuredSerializerPartialInvalid
			case "result":
				stage = structuredResultInvalid
			}
			if header.Type == "assistant" {
				return fail(decodeErr, stage, line)
			}
			return fail(structuredError("structured_protocol_invalid", stage), stage, line)
		}
		noteStructuredBlocks(&diagnostic, record)
		if record.ParentToolUseID != "" || record.SubagentType != "" {
			return fail(toolStreamFailure(), structuredExecutionRejected, line)
		}
		switch record.Type {
		case "system":
			stage := structuredSystemFrameInvalid
			if record.Subtype == "init" {
				stage = structuredInitFrameInvalid
			}
			if err := systems.write(&clean, line, record); err != nil {
				return fail(err, stage, line)
			}
			continue
		case "assistant":
			var content []block
			for _, b := range record.Message.Content {
				switch b.Type {
				case "tool_use":
					if b.Name != structuredOutputTool || b.ID == "" {
						return fail(toolStreamFailure(), structuredExecutionRejected, line)
					}
					serializerIDs[b.ID] = true
					if len(b.Input) > structuredResponseBytes {
						return fail(structuredError("structured_response_too_large", structuredResponseOversized), structuredResponseOversized, line)
					}
					if b.Input.Kind() != '{' {
						return fail(structuredError("structured_protocol_invalid", structuredSerializerFrameInvalid), structuredSerializerFrameInvalid, line)
					}
					// Helper input is an internal representation, not an Action.
					// Failed serializer attempts/retries may differ. Only the public
					// final result passes the Host envelope/registry/schema checks.
				case "text", "thinking", "redacted_thinking":
					// Do not publish intermediate protocol text or private reasoning.
				default:
					return fail(toolStreamFailure(), structuredExecutionRejected, line)
				}
			}
			record.Message.Content = content
			// SDK helper serialization may involve more than one assistant frame.
			// Its replay ID is private and carries no canonical message identity.
			record.Message.ID = "unreal-structured-serializer"
			if !publicID(record.Message.Model) {
				// Opaque/synthetic helper metadata is not a public inference
				// identity. Keep the validated init observation instead.
				var init streamRecord
				if json.Unmarshal(systems.init, &init) != nil || !publicID(init.Model) {
					return fail(structuredError("structured_protocol_invalid", structuredInitFrameInvalid), structuredInitFrameInvalid, line)
				}
				record.Message.Model = init.Model
			}
			if record.Message.StopReason == "tool_use" {
				if len(serializerIDs) == 0 {
					return fail(toolStreamFailure(), structuredExecutionRejected, line)
				}
				record.Message.StopReason = "end_turn"
			}
		case "stream_event":
			var position struct {
				Event struct {
					Index *int `json:"index"`
					Delta struct {
						Partial *string `json:"partial_json"`
					} `json:"delta"`
				} `json:"event"`
			}
			if json.Unmarshal(line, &position) != nil {
				return fail(structuredError("structured_protocol_invalid", structuredSerializerPartialInvalid), structuredSerializerPartialInvalid, line)
			}
			index := position.Event.Index
			if strings.HasPrefix(record.Event.Type, "content_block_") && (index == nil || *index < 0) {
				return fail(structuredError("structured_protocol_invalid", structuredSerializerPartialInvalid), structuredSerializerPartialInvalid, line)
			}
			b := record.Event.ContentBlock
			if record.Event.Type == "content_block_start" {
				if activeBlocks[*index] {
					return fail(structuredError("structured_protocol_invalid", structuredSerializerPartialInvalid), structuredSerializerPartialInvalid, line)
				}
				activeBlocks[*index] = true
			}
			if record.Event.Type == "content_block_delta" || record.Event.Type == "content_block_stop" {
				if !activeBlocks[*index] {
					return fail(structuredError("structured_protocol_invalid", structuredSerializerPartialInvalid), structuredSerializerPartialInvalid, line)
				}
			}
			if record.Event.Type == "content_block_start" && b.Type == "tool_use" {
				if b.Name != structuredOutputTool || !publicID(b.ID) {
					return fail(toolStreamFailure(), structuredExecutionRejected, line)
				}
				if _, exists := serializerBlocks[*index]; exists || len(b.Input) != 0 && b.Input.Kind() != '{' {
					return fail(structuredError("structured_protocol_invalid", structuredSerializerPartialInvalid), structuredSerializerPartialInvalid, line)
				}
				serializerBlocks[*index] = len(b.Input)
				// message_delta(tool_use) precedes the complete assistant frame.
				// Only the already checked serializer start can authorize that stop.
				serializerIDs[b.ID] = true
				record.Event.ContentBlock = block{Type: "text"}
			}
			if record.Event.Delta.Type == "input_json_delta" {
				if record.Event.Type != "content_block_delta" || index == nil || position.Event.Delta.Partial == nil {
					return fail(structuredError("structured_protocol_invalid", structuredSerializerPartialInvalid), structuredSerializerPartialInvalid, line)
				}
				bytes, exists := serializerBlocks[*index]
				if !exists {
					return fail(toolStreamFailure(), structuredExecutionRejected, line)
				}
				bytes += len(*position.Event.Delta.Partial)
				if bytes > structuredResponseBytes {
					return fail(structuredError("structured_response_too_large", structuredResponseOversized), structuredResponseOversized, line)
				}
				serializerBlocks[*index] = bytes
				diagnostic.SerializerInputBytes = bytes
				record.Event.Delta.Type = "text_delta"
			}
			if record.Event.Type == "content_block_stop" {
				delete(serializerBlocks, *index)
				delete(activeBlocks, *index)
			}
			if record.Event.Delta.StopReason == "tool_use" {
				if len(serializerIDs) == 0 {
					return fail(toolStreamFailure(), structuredExecutionRejected, line)
				}
				record.Event.Delta.StopReason = "end_turn"
			}
			record.Event.Delta.Text = ""
			if err := textOnlyEvent(record.Event); err != nil {
				return fail(err, structuredSerializerPartialInvalid, line)
			}
		case "user":
			if len(record.Message.Content) == 0 {
				return fail(toolStreamFailure(), structuredExecutionRejected, line)
			}
			for _, b := range record.Message.Content {
				if b.Type != "tool_result" || !serializerIDs[b.ToolUseID] {
					return fail(toolStreamFailure(), structuredExecutionRejected, line)
				}
			}
			continue // output serialization receipt, never an Unreal receipt
		case "result":
			var execution struct {
				Deferred jsontext.Value `json:"deferred_tool_use"`
			}
			if json.Unmarshal(line, &execution) != nil || len(execution.Deferred) != 0 && execution.Deferred.Kind() != 'n' {
				return fail(toolStreamFailure(), structuredExecutionRejected, line)
			}
			// SDKResultSuccess exposes structured_output, not a tool execution
			// receipt. Serializer receipts are accepted only in correlated user
			// frames above. Do not hide an unclassified execution field by clearing
			// it while normalizing an otherwise schema-valid public result.
			if len(record.ToolUseResult) != 0 && record.ToolUseResult.Kind() != 'n' {
				return fail(toolStreamFailure(), structuredExecutionRejected, line)
			}
			if len(activeBlocks) != 0 {
				return fail(structuredError("structured_protocol_invalid", structuredStreamIncomplete), structuredStreamIncomplete, line)
			}
			if record.IsError || record.Subtype != "success" {
				if record.Subtype == "error_max_turns" {
					return fail(structuredError("structured_serializer_round_limit", structuredSerializerRoundLimit), structuredSerializerRoundLimit, line)
				}
				if record.Subtype == "error_max_structured_output_retries" {
					return fail(structuredError("structured_protocol_invalid", structuredSchemaRejected), structuredSchemaRejected, line)
				}
				return fail(streamFailure(record), structuredResultInvalid, line)
			}
			var result struct {
				Output jsontext.Value `json:"structured_output"`
			}
			if json.Unmarshal(line, &result) != nil {
				return fail(structuredError("structured_protocol_invalid", structuredResultInvalid), structuredResultInvalid, line)
			}
			diagnostic.StructuredOutputPresent = len(result.Output) != 0
			diagnostic.StructuredOutputNull = result.Output.Kind() == 'n'
			diagnostic.StructuredOutputBytes = len(result.Output)
			var err error
			proposal, err = schema.parse(result.Output)
			if err != nil {
				return fail(err, structuredResultInvalid, line)
			}
			diagnostic.SchemaChecked, diagnostic.SchemaValid = true, true
			record.Result = ""
			if err = writeBridgeFrame(&clean, record); err != nil {
				return fail(err, structuredResultInvalid, line)
			}
			response, err = parseStream(bytes.NewReader(clean.Bytes()), llm.RequestOptions{})
			if err != nil {
				return fail(err, structuredResultInvalid, line)
			}
			response.ID = ""
			response.Output = nil
			finished = true
			// Close the request stream so the CLI can terminate. Drain and validate
			// stdout before permitting an Action; a trailing execution event or
			// broken stream must not be hidden by an earlier success result.
			if finish != nil {
				finish()
			}
			continue
		case "tool_progress", "tool_use_summary":
			if !isSerializerMetadata(line, record.Type, serializerIDs) {
				return fail(toolStreamFailure(), structuredExecutionRejected, line)
			}
			continue // Correlated output-serializer metadata; never a Host tool.
		case "control_request":
			// No SDK MCP or permission callbacks are part of this protocol. A
			// required permission callback cannot be approved by this adapter.
			return fail(structuredError("structured_unavailable", structuredUnexpectedControlFrame), structuredUnexpectedControlFrame, line)
		case "control_response", "control_cancel_request":
			return fail(structuredError("structured_protocol_invalid", structuredUnexpectedControlFrame), structuredUnexpectedControlFrame, line)
		case "error":
			return fail(streamFailure(record), structuredResultInvalid, line)
		case "rate_limit_event":
			if record.RateLimit.Status != "allowed" && record.RateLimit.Status != "allowed_warning" {
				code := "structured_protocol_invalid"
				if record.RateLimit.Status == "rejected" {
					code = "rate_limited"
				}
				return fail(structuredError(code, structuredStreamFrameInvalid), structuredStreamFrameInvalid, line)
			}
		default:
			return fail(structuredError("structured_protocol_invalid", structuredStreamFrameInvalid), structuredStreamFrameInvalid, line)
		}
		if len(record.ToolUseResult) > 0 && string(record.ToolUseResult) != "null" {
			return fail(toolStreamFailure(), structuredExecutionRejected, line)
		}
		if err := writeBridgeFrame(&clean, record); err != nil {
			return fail(err, structuredStreamFrameInvalid, line)
		}
	}
	if scanner.Err() != nil || !finished {
		stage, code := structuredStreamIncomplete, "structured_protocol_invalid"
		if scanner.Err() == bufio.ErrTooLong {
			stage, code = structuredResponseOversized, "structured_response_too_large"
		}
		return fail(structuredError(code, stage), stage, nil)
	}
	return proposal, response, nil
}

func writeStructuredSystem(w io.Writer, line []byte, record streamRecord) error {
	// Retain the original system shape for the shared validator. Re-encoding a
	// streamRecord would drop metadata fields and execution-linked markers.
	if len(record.Tools) == 0 {
		return writeBridgeFrame(w, jsontext.Value(line))
	}
	if len(record.Tools) != 1 || record.Tools[0] != structuredOutputTool {
		return initializationFailure(streamInitToolsNonempty, len(record.Tools))
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil {
		return structuredError("structured_protocol_invalid", structuredInitFrameInvalid)
	}
	fields["tools"] = jsontext.Value(`[]`)
	return writeBridgeFrame(w, fields)
}

// Reuse the shared isolation validator immediately, so the diagnostic frame is
// the offending system frame, not a later result. Only the bounded init frame
// is retained: each check examines init + current metadata + a local sentinel.
type structuredSystemValidator struct{ init []byte }

func (s *structuredSystemValidator) write(w io.Writer, line []byte, record streamRecord) error {
	var normalized bytes.Buffer
	if err := writeStructuredSystem(&normalized, line, record); err != nil {
		return err
	}
	validation := append(append([]byte(nil), s.init...), normalized.Bytes()...)
	validation = append(validation, []byte("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false}\n")...)
	if _, err := parseStream(bytes.NewReader(validation), llm.RequestOptions{}); err != nil {
		return err
	}
	if record.Subtype == "init" {
		s.init = append([]byte(nil), normalized.Bytes()...)
	}
	_, err := w.Write(normalized.Bytes())
	return err
}

func isStructuredKeepAlive(line []byte) bool {
	var fields map[string]jsontext.Value
	return json.Unmarshal(line, &fields) == nil && len(fields) == 1 && bytes.Equal(fields["type"], []byte(`"keep_alive"`))
}

// These SDK metadata frames can describe StructuredOutput itself. Accept only
// the pinned public shapes, correlated exclusively to checked serializer IDs.
// No arbitrary tool progress/summary, task, or unknown extra field is allowed.
func isSerializerMetadata(line []byte, typ string, ids map[string]bool) bool {
	var fields map[string]jsontext.Value
	if json.Unmarshal(line, &fields) != nil {
		return false
	}
	for name, raw := range fields {
		switch name {
		case "type", "uuid", "session_id":
			if raw.Kind() != '"' {
				return false
			}
		case "parent_tool_use_id", "subagent_type", "task_id":
			if typ != "tool_progress" {
				return false
			}
			var value string
			if raw.Kind() != 'n' && (json.Unmarshal(raw, &value) != nil || value != "") {
				return false
			}
		case "tool_name", "tool_use_id", "elapsed_time_seconds", "heartbeat":
			if typ != "tool_progress" {
				return false
			}
		case "summary", "preceding_tool_use_ids":
			if typ != "tool_use_summary" {
				return false
			}
		default:
			return false
		}
	}
	if typ == "tool_progress" {
		var progress struct {
			Name      string   `json:"tool_name"`
			ID        string   `json:"tool_use_id"`
			Elapsed   *float64 `json:"elapsed_time_seconds"`
			Heartbeat *bool    `json:"heartbeat"`
		}
		return json.Unmarshal(line, &progress) == nil && progress.Name == structuredOutputTool && ids[progress.ID] && progress.Elapsed != nil && *progress.Elapsed >= 0
	}
	var summary struct {
		Summary *string  `json:"summary"`
		IDs     []string `json:"preceding_tool_use_ids"`
	}
	if json.Unmarshal(line, &summary) != nil || summary.Summary == nil || len(summary.IDs) == 0 {
		return false
	}
	for _, id := range summary.IDs {
		if !ids[id] {
			return false
		}
	}
	return true
}

func structuredDenial(err error) error {
	if e, ok := err.(*Error); ok && e.Code == "permission_denied" {
		return &Error{Code: "structured_unavailable", permissionReason: e.permissionReason, structuredStage: structuredPermissionDenied}
	}
	return err
}

// The SDK initialize.jsonSchema field is the official equivalent of
// --json-schema/outputFormat. Send it over stdin; no schema or prompt in argv.
func runStructuredProtocol(r io.Reader, w io.Writer, schema *actionSchema, input string, probe bool) (actionProposal, llm.Response, error) {
	initialize, user, err := structuredProtocolFrames(schema, input, probe)
	if err != nil {
		return actionProposal{}, llm.Response{}, err
	}
	if err := writeBridgeFrame(w, initialize); err != nil {
		return actionProposal{}, llm.Response{}, annotateStructured(err, structuredInitFrameInvalid, nil, StructuredDiagnostics{})
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 8192), 2<<20)
	var prefix bytes.Buffer
	systems := structuredSystemValidator{}
	fail := func(err error, stage structuredStage, line []byte) (actionProposal, llm.Response, error) {
		return actionProposal{}, llm.Response{}, annotateStructured(err, stage, line, StructuredDiagnostics{})
	}
	for n := 0; n < 256 && scanner.Scan(); n++ {
		line := scanner.Bytes()
		if len(line) > 1<<20 {
			return fail(structuredError("structured_response_too_large", structuredResponseOversized), structuredResponseOversized, line)
		}
		if err := validateStructuredJSON(line); err != nil {
			return fail(err, structuredInitFrameInvalid, line)
		}
		if known, err := permissionDeniedEvent(line, false, nil); known {
			return fail(structuredDenial(err), structuredPermissionDenied, line)
		}
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Subtype string `json:"subtype"`
				ID      string `json:"request_id"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &frame) != nil {
			return fail(structuredError("structured_protocol_invalid", structuredInitFrameInvalid), structuredInitFrameInvalid, line)
		}
		if isStructuredKeepAlive(line) {
			continue
		}
		if frame.Type == "system" {
			var record streamRecord
			if json.Unmarshal(line, &record) != nil {
				return fail(structuredError("structured_protocol_invalid", structuredInitFrameInvalid), structuredInitFrameInvalid, line)
			}
			if err := systems.write(&prefix, line, record); err != nil {
				return fail(err, structuredInitFrameInvalid, line)
			}
			if prefix.Len() > 1<<20 {
				return fail(structuredError("structured_response_too_large", structuredResponseOversized), structuredResponseOversized, line)
			}
			continue
		}
		if frame.Type != "control_response" {
			return fail(structuredError("structured_protocol_invalid", structuredUnexpectedPreinitFrame), structuredUnexpectedPreinitFrame, line)
		}
		if err := validateStructuredControlResponse(line); err != nil {
			return fail(err, structuredInitFrameInvalid, line)
		}
		if frame.Response.ID != structuredInitID || frame.Response.Subtype != "success" {
			code := "structured_protocol_invalid"
			if frame.Response.Subtype == "error" && frame.Response.ID == structuredInitID {
				code = "structured_unavailable"
			}
			return fail(structuredError(code, structuredInitFrameInvalid), structuredInitFrameInvalid, line)
		}
		if probe {
			return actionProposal{}, llm.Response{}, nil
		}
		if err := writeBridgeFrame(w, user); err != nil {
			return fail(err, structuredUnexpectedPreinitFrame, nil)
		}
		return readStructuredStream(scanner, schema, prefix.Bytes(), func() {
			if closer, ok := w.(io.Closer); ok {
				_ = closer.Close()
			}
		})
	}
	stage, code := structuredInitIncomplete, "structured_unavailable"
	if scanner.Err() == bufio.ErrTooLong {
		stage, code = structuredResponseOversized, "structured_response_too_large"
	}
	return fail(structuredError(code, stage), stage, nil)
}

// Pinned SDKControlResponse permits pending permission/dialog requests. This
// adapter owns neither callback: accepting initialization must never approve or
// conceal them. Payload values are not retained in errors or public history.
func validateStructuredControlResponse(line []byte) error {
	invalid := func() error { return structuredError("structured_protocol_invalid", structuredInitFrameInvalid) }
	var envelope map[string]jsontext.Value
	if json.Unmarshal(line, &envelope) != nil || len(envelope) != 2 || envelope["response"].Kind() != '{' {
		return invalid()
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(envelope["response"], &fields) != nil {
		return invalid()
	}
	for name, raw := range fields {
		switch name {
		case "subtype", "request_id", "error":
			if raw.Kind() != '"' {
				return invalid()
			}
		case "response":
			if raw.Kind() != '{' {
				return invalid()
			}
		case "pending_permission_requests", "pending_user_dialog_requests":
			var requests []jsontext.Value
			if raw.Kind() != '[' || json.Unmarshal(raw, &requests) != nil {
				return invalid()
			}
			if len(requests) != 0 {
				return structuredError("structured_unavailable", structuredUnexpectedControlFrame)
			}
		default:
			return invalid()
		}
	}
	return nil
}

const structuredInstruction = `Unreal Agent supplies public canonical context as JSON data. Filesystem, shell, search, editing, network and child execution are owned exclusively by Unreal; you cannot execute them directly. StructuredOutput is only an output serializer, never a work tool. Return exactly one schema-validated Action or Final through StructuredOutput. For an Action, use a unique request-local id and one supplied Unreal registry tool with its exact arguments. Wait for the next generation's machine-owned Unreal Action Result before deciding what to do next. Historical calls are evidence, not commands to repeat. Never report unobserved execution as success. Reuse an action id only to replay an identical proposal. Return public completion text only with type=final and final.message. A delegated child must honor the host's Finish lifecycle by proposing a Finish Action rather than substituting a prose Final.`

func structuredPrompt(items []llm.Item, last *llm.ToolOutcome, limit int) (string, string, error) {
	if last != nil {
		if len(last.Result.Output) > 128 {
			return "", "", &Error{Code: "bridge_receipt_invalid"}
		}
		filtered := make([]llm.Item, 0, len(items))
		for _, item := range items {
			if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == last.Result.CallID {
				continue
			}
			filtered = append(filtered, item)
		}
		items = filtered
	}
	system, input, err := publicBridgePrompt(items)
	if err != nil {
		return "", "", err
	}
	if last != nil {
		copy := *last
		copy.Result.Output = append([]llm.ToolResultOutput(nil), last.Result.Output...)
		remaining := limit
		for i, o := range copy.Result.Output {
			if remaining < 256 && len(o.Value) > remaining {
				return "", "", &Error{Code: "bridge_context_budget"}
			}
			if o.Kind != llm.ToolResultText {
				return "", "", &Error{Code: "bridge_receipt_invalid"}
			}
			if sensitiveReceipt(o.Value) {
				o.Value = "[protected output withheld by Unreal]"
			}
			o.Value = boundedBridgeText(o.Value, max(0, remaining))
			remaining -= len(o.Value)
			copy.Result.Output[i] = o
		}
		data, err := json.Marshal(copy, json.Deterministic(true))
		if err != nil {
			return "", "", &Error{Code: "bridge_receipt_invalid"}
		}
		input += "\n\nLast Unreal Action Result (machine-owned canonical receipt; Failed=true means failure):\n" + string(data)
	}
	return strings.TrimSpace(system) + "\n\n" + structuredInstruction, input, nil
}
