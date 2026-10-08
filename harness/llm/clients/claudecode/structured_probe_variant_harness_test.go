//go:build linux || darwin

package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Schema variants deliberately include incomplete, non-executable envelopes.
// Validate their authoritative result against the selected schema without ever
// constructing an Action, consulting the Registry, or calling Respond.
func validateStructuredProbeVariantOutput(variant probeSchemaVariant, raw jsontext.Value) structuredProbeDiagnostic {
	detail := StructuredDiagnostics{StructuredOutputPresent: len(raw) != 0, StructuredOutputNull: raw.Kind() == 'n', StructuredOutputBytes: len(raw)}
	failure := func(code string, stage structuredStage) structuredProbeDiagnostic {
		return classifyStructuredProbe(actionProposal{}, llm.Response{}, annotateStructured(structuredError(code, stage), stage, nil, detail))
	}
	if len(raw) == 0 {
		return failure("structured_protocol_invalid", structuredResultMissingOutput)
	}
	if raw.Kind() == 'n' {
		return failure("structured_protocol_invalid", structuredResultNullOutput)
	}
	if len(raw) > structuredResponseBytes {
		return failure("structured_response_too_large", structuredResponseOversized)
	}
	if err := validateStructuredJSON(raw); err != nil {
		return classifyStructuredProbe(actionProposal{}, llm.Response{}, annotateStructured(err, structuredJSONDecodeFailed, nil, detail))
	}
	detail.SchemaChecked = true
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil || variant.Schema.validator.Validate(value) != nil {
		return failure("structured_protocol_invalid", structuredSchemaRejected)
	}
	detail.SchemaValid = true
	if variant.Strict != nil {
		if _, err := variant.Strict.parse(raw); err != nil {
			return classifyStructuredProbe(actionProposal{}, llm.Response{}, err)
		}
	}
	var envelope struct {
		Type   string         `json:"type"`
		Action jsontext.Value `json:"action"`
		Final  *struct {
			Message string `json:"message"`
		} `json:"final"`
	}
	if json.Unmarshal(raw, &envelope, json.RejectUnknownMembers(true)) != nil {
		return failure("structured_protocol_invalid", structuredActionEnvelopeInvalid)
	}
	d := structuredProbeDiagnostic{StructuredOutputPresent: true, SchemaChecked: true, SchemaValid: true}
	switch {
	case envelope.Type == "final" && envelope.Final != nil:
		d.Outcome = probeValidatedFinal
		if len(envelope.Action) != 0 {
			// The frozen 329-byte B schema still requires type=final and final.
			// Its optional action object is an inert schema witness, never a
			// production Action/Final union or an executable tool proposal.
			if variant.Name != "B" || envelope.Action.Kind() != '{' {
				return failure("structured_protocol_invalid", structuredActionEnvelopeInvalid)
			}
			d.Outcome = probeValidatedAction
		}
		if envelope.Final.Message != structuredProbeFinalMessage {
			d.Reason = probeFinalMessageMismatch
		}
	case envelope.Type == "action" && envelope.Action.Kind() == '{' && envelope.Final == nil:
		d.Outcome = probeValidatedAction
	case variant.Name == "C2" && envelope.Type == "action" && len(envelope.Action) == 0 && envelope.Final == nil:
		// C2's minimum second branch is only {type:action}. It has no work
		// payload, identity or tool and is solely a union-validation witness.
		d.Outcome = probeValidatedAction
	default:
		return failure("structured_protocol_invalid", structuredActionEnvelopeInvalid)
	}
	return d
}

// Keep all production stream-security checks, including the complete tail, for
// these schema-only witnesses. Only result.structured_output is projected to an
// inert, fixed Final for the execution-envelope parser, AFTER strict JSON and
// variant schema validation. The original output contributes only safe outcome
// flags. No other field/frame is removed; any protocol/execution failure wins.
// Invalid outputs are projected too solely to finish the security scan, but
// their typed rejection is retained and can never become an accepted outcome.
// This entire reader is test-only and unreachable from the production bridge.
type structuredProbeVariantReader struct {
	scanner *bufio.Scanner
	variant probeSchemaVariant
	pending []byte
	result  *structuredProbeDiagnostic
	bytes   int
	frames  int
	failure error
}

func newStructuredProbeVariantReader(r io.Reader, variant probeSchemaVariant) *structuredProbeVariantReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 8192), 2<<20)
	return &structuredProbeVariantReader{scanner: scanner, variant: variant}
}

func (r *structuredProbeVariantReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				if err == bufio.ErrTooLong {
					r.failure = structuredError("structured_response_too_large", structuredResponseOversized)
				}
				return 0, err
			}
			return 0, io.EOF
		}
		line := r.scanner.Bytes()
		r.bytes += len(line)
		r.frames++
		if r.bytes > 32<<20 || r.frames > 65536 {
			r.failure = structuredError("structured_response_too_large", structuredResponseOversized)
			return 0, r.failure
		}
		// Never remarshal malformed/duplicate-field JSON: preserve it verbatim
		// so the shared fail-closed protocol reader rejects its original shape.
		var header struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
		}
		if validateStructuredJSON(line) == nil && json.Unmarshal(line, &header) == nil && header.Type == "result" && header.Subtype == "success" && !header.IsError {
			var fields map[string]jsontext.Value
			if json.Unmarshal(line, &fields) == nil {
				d := validateStructuredProbeVariantOutput(r.variant, fields["structured_output"])
				r.result = &d
				fields["structured_output"] = jsontext.Value(structuredProbeFinal)
				projected, err := json.Marshal(fields, json.Deterministic(true))
				if err != nil {
					return 0, structuredError("structured_protocol_invalid", structuredResultInvalid)
				}
				line = projected
			}
		}
		r.pending = append(append(r.pending[:0], line...), '\n')
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *structuredProbeVariantReader) diagnostic(response llm.Response, err error) structuredProbeDiagnostic {
	if r.failure != nil {
		err = r.failure
	}
	if err != nil {
		d := classifyStructuredProbe(actionProposal{}, llm.Response{}, err)
		if r.result != nil {
			// The security parser saw the inert projection. Report the actual
			// authoritative value's flags even when a trailing event rejects it.
			d.StructuredOutputPresent, d.StructuredOutputNull = r.result.StructuredOutputPresent, r.result.StructuredOutputNull
			d.SchemaChecked, d.SchemaValid = r.result.SchemaChecked, r.result.SchemaValid
			if d.Details != nil {
				d.Details.StructuredOutputPresent, d.Details.StructuredOutputNull = d.StructuredOutputPresent, d.StructuredOutputNull
				d.Details.SchemaChecked, d.Details.SchemaValid = d.SchemaChecked, d.SchemaValid
			}
		}
		return d
	}
	if r.result == nil {
		return classifyStructuredProbe(actionProposal{}, llm.Response{}, structuredError("structured_protocol_invalid", structuredResultMissingOutput))
	}
	d := *r.result
	if len(response.Output) != 0 {
		d.Reason = probeUnexpectedResponseOutput
	}
	return d
}

type structuredProbeVariantGenerator func(context.Context, structuredProbeInvocation, probeSchemaVariant) structuredProbeDiagnostic

func runStructuredProbeVariant(ctx context.Context, variant probeSchemaVariant, model llm.Model, generate structuredProbeVariantGenerator) structuredProbeReport {
	invocation := newStructuredProbeInvocation(model, variant.Schema)
	report := structuredProbeReport{Variant: variant.Name, SchemaBytes: len(variant.Schema.document), SchemaSHA256: fmt.Sprintf("%x", sha256.Sum256(variant.Schema.document))}
	generationCtx, cancel := context.WithTimeout(ctx, invocation.Timeout)
	defer cancel()
	report.Diagnostic = generate(generationCtx, invocation, variant)
	return report
}

// Only the opt-in probe/test invokes this isolated schema adapter. It reuses
// buildStructuredRequest, process isolation and runStructuredProtocol verbatim.
// Its return type contains no proposal/ToolCall and no execution callback exists.
func structuredProbeVariantGeneration(ctx context.Context, path string, env []string, invocation structuredProbeInvocation, variant probeSchemaVariant) structuredProbeDiagnostic {
	failure := func(err error) structuredProbeDiagnostic {
		return classifyStructuredProbe(actionProposal{}, llm.Response{}, err)
	}
	dir, err := os.MkdirTemp("", "unreal-claude-structured-")
	if err != nil {
		return failure(&Error{Code: "subprocess_failure"})
	}
	defer os.RemoveAll(dir)
	systemPath := filepath.Join(dir, "system.txt")
	if os.WriteFile(systemPath, []byte(invocation.System), 0600) != nil {
		return failure(&Error{Code: "subprocess_failure"})
	}
	request, err := buildStructuredRequest(invocation.Model, env, variant.Schema, systemPath, invocation.Input, false)
	if err != nil {
		return failure(err)
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, path, request.Arguments...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, request.Environment, io.Discard
	if err = isolateProcess(cmd); err != nil {
		return failure(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		return failure(&Error{Code: "subprocess_failure"})
	}
	defer reader.Close()
	defer output.Close()
	stdin, writer, err := os.Pipe()
	if err != nil {
		return failure(&Error{Code: "subprocess_failure"})
	}
	defer stdin.Close()
	defer writer.Close()
	cmd.Stdin, cmd.Stdout = stdin, output
	if err = cmd.Start(); err != nil {
		return failure(&Error{Code: "subprocess_failure"})
	}
	stdin.Close()
	output.Close()
	done, parsed := make(chan error, 1), make(chan struct{})
	go func() {
		err := cmd.Wait()
		_ = killProcessGroup(cmd)
		select {
		case <-parsed:
		case <-processCtx.Done():
			reader.Close()
		case <-time.After(2 * time.Second):
			reader.Close()
		}
		done <- err
	}()
	projection := newStructuredProbeVariantReader(reader, variant)
	_, response, streamErr := runStructuredProtocol(projection, writer, variant.Schema, invocation.Input, false)
	close(parsed)
	writer.Close()
	var waitErr error
	natural := false
	select {
	case waitErr = <-done:
		natural = true
	case <-ctx.Done():
		cancel()
		waitErr = <-done
	case <-time.After(100 * time.Millisecond):
		cancel()
		waitErr = <-done
	}
	cancel()
	reader.Close()
	if ctx.Err() != nil {
		return failure(ctx.Err())
	}
	if streamErr == nil && natural && waitErr != nil {
		streamErr = &Error{Code: "subprocess_failure"}
	}
	return projection.diagnostic(response, streamErr)
}
