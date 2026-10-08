package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
)

func initializationFailure(reason streamFailureReason, count int) *Error {
	return &Error{Code: "tools_unsupported", toolReason: toolStreamInitialization, streamReason: reason, streamCount: count}
}

func malformedStream(reason streamFailureReason) *Error {
	return &Error{Code: "malformed_stream", streamReason: reason}
}

func toolStreamFailure() *Error {
	return &Error{Code: "tools_unsupported", toolReason: toolStreamOutput, streamReason: streamToolExecution}
}

// Raw fields are inspected only while classifying an existing rejection. They
// are local to the parser and never attached to Error, returned or logged.
type diagnosticInitShape struct {
	Type    jsontext.Value `json:"type"`
	Subtype jsontext.Value `json:"subtype"`
	Model   jsontext.Value `json:"model"`
	Tools   jsontext.Value `json:"tools"`
	MCP     jsontext.Value `json:"mcp_servers"`
}

func initRequiredFieldFailure(line []byte, v streamRecord) *Error {
	var shape diagnosticInitShape
	if json.Unmarshal(line, &shape) != nil {
		return malformedStream(streamInitUnknownShape)
	}
	var raw jsontext.Value
	switch {
	case v.Tools == nil:
		raw = shape.Tools
	case v.MCP == nil:
		raw = shape.MCP
	case !publicID(v.Model):
		raw = shape.Model
	}
	switch {
	case len(raw) == 0:
		return malformedStream(streamInitMissingRequired)
	case raw.Kind() == 'n':
		return malformedStream(streamInitNullRequired)
	case !publicID(v.Model):
		return malformedStream(streamInitModelInvalid)
	default:
		return malformedStream(streamInitUnknownShape)
	}
}

func streamDecodeFailure(line []byte, decodeErr error, initialized bool) *Error {
	var shape diagnosticInitShape
	if json.Unmarshal(line, &shape) != nil || len(shape.Type) == 0 {
		if !initialized {
			return malformedStream(streamInitUnknownShape)
		}
		return malformedStream(0)
	}
	var typ, subtype string
	if json.Unmarshal(shape.Type, &typ) != nil || json.Unmarshal(shape.Subtype, &subtype) != nil {
		if typ == "system" && initialized {
			return unknownSystemShape(line)
		}
		if !initialized || typ == "system" {
			return malformedStream(streamInitWrongFieldType)
		}
		return malformedStream(0)
	}
	if typ == "system" && subtype == "status" {
		return malformedStream(streamStatusWrongFieldType)
	}
	if typ != "system" || subtype != "init" {
		if typ == "system" {
			return unknownSystemShape(line)
		}
		return malformedStream(0)
	}
	var semantic *json.SemanticError
	if errors.As(decodeErr, &semantic) {
		// Inspect only known pointer prefixes. SemanticError can contain raw
		// values and arbitrary keys; never retain it, unwrap it or format it.
		pointer := string(semantic.JSONPointer)
		switch {
		case pointer == "/agents" || strings.HasPrefix(pointer, "/agents/"):
			return malformedStream(streamInitAgentsInvalid)
		case pointer == "/skills" || strings.HasPrefix(pointer, "/skills/"):
			return malformedStream(streamInitSkillsInvalid)
		}
	}
	return malformedStream(streamInitWrongFieldType)
}
