//go:build linux || darwin

package claudecode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const (
	structuredProbeFinalMessage = "adapter schema probe OK"
	structuredProbeSystem       = `This is an output serialization compatibility probe. StructuredOutput is the only permitted internal serializer. Do not request or execute any work. Return exactly this Final JSON value through StructuredOutput: {"type":"final","final":{"message":"adapter schema probe OK"}}. Do not add any other fields or change the message string.`
	structuredProbeInput        = "Return the specified Final. No Action, filesystem, shell, network, child or project work is requested."
	structuredProbeTimeout      = 5 * time.Minute
)

// This harness is test-only, including when the operator explicitly opts into a
// real adapter probe. A maximum level is a loop bound, never provider input.
// No Host, tool callback, action controller, Session or executor is reachable.
type structuredProbeInvocation struct {
	Model         llm.Model
	Schema        *actionSchema
	System, Input string
	Timeout       time.Duration
}

func newStructuredProbeInvocation(model llm.Model, schema *actionSchema) structuredProbeInvocation {
	return structuredProbeInvocation{Model: model, Schema: schema, System: structuredProbeSystem, Input: structuredProbeInput, Timeout: structuredProbeTimeout}
}

// Selection is harness-only. Reject conflicting opt-ins before authentication,
// request construction or subprocess launch; zero/zero keeps ordinary tests off.
func selectStructuredProbeLevels(ladder, exact int) ([]int, error) {
	if ladder < 0 || ladder > 5 || exact < 0 || exact > 5 || ladder != 0 && exact != 0 {
		return nil, errors.New("invalid adapter probe level selection: use either levels=1..5 or exact-level=1..5")
	}
	if exact != 0 {
		return []int{exact}, nil
	}
	var levels []int
	for level := 1; level <= ladder; level++ {
		levels = append(levels, level)
	}
	return levels, nil
}

type structuredProbeSelection struct {
	Levels  []int
	Variant string
}

// The variant name is a harness selector, never provider input. A variant run
// visits exactly one schema and is mutually exclusive with both level modes.
func selectStructuredProbe(ladder, exact int, variant string) (structuredProbeSelection, error) {
	levels, err := selectStructuredProbeLevels(ladder, exact)
	if err != nil {
		return structuredProbeSelection{}, err
	}
	if variant == "" {
		return structuredProbeSelection{Levels: levels}, nil
	}
	validVariant := len(variant) == 1 && variant[0] >= 'A' && variant[0] <= 'F' || variant == "C1" || variant == "C2" || isTransportProbeVariant(variant)
	if ladder != 0 || exact != 0 || !validVariant {
		return structuredProbeSelection{}, errors.New("invalid adapter probe selection: use one variant A..F/C1/C2/T1..T7, levels=1..5 or exact-level=1..5")
	}
	return structuredProbeSelection{Variant: variant}, nil
}

func isTransportProbeVariant(name string) bool {
	return len(name) == 2 && name[0] == 'T' && name[1] >= '1' && name[1] <= '7'
}

type probeOutcome uint8

const (
	probeProtocolFailure probeOutcome = iota
	probeValidatedFinal
	probeValidatedAction
	probeStructuredMissing
	probeStructuredNull
	probeSchemaInvalid
	probeEnvelopeInvalid
	probeProviderFailure
)

func (o probeOutcome) String() string {
	switch o {
	case probeValidatedFinal:
		return "validated_final"
	case probeValidatedAction:
		return "validated_action"
	case probeStructuredMissing:
		return "structured_missing"
	case probeStructuredNull:
		return "structured_null"
	case probeSchemaInvalid:
		return "schema_invalid"
	case probeEnvelopeInvalid:
		return "envelope_invalid"
	case probeProviderFailure:
		return "provider_failure"
	default:
		return "protocol_failure"
	}
}

type probeValidationReason uint8

const (
	probeValidationOK probeValidationReason = iota
	probeUnexpectedAction
	probeFinalMessageMismatch
	probeUnexpectedResponseOutput
	probeInvalidEnvelope
	probeTypedFailure
	probeTimeout
	probeCanceled
	probeUnclassifiedFailure
)

func (r probeValidationReason) String() string {
	switch r {
	case probeValidationOK:
		return "validated"
	case probeUnexpectedAction:
		return "unexpected_action"
	case probeFinalMessageMismatch:
		return "final_message_mismatch"
	case probeUnexpectedResponseOutput:
		return "unexpected_response_output"
	case probeInvalidEnvelope:
		return "invalid_envelope"
	case probeTimeout:
		return "timeout"
	case probeCanceled:
		return "canceled"
	case probeUnclassifiedFailure:
		return "unclassified_failure"
	default:
		return "typed_failure"
	}
}

// Only closed enums, validation booleans and the adapter's existing safe shape
// projection survive classification. Neither the proposal, response, error
// body nor its IDs are retained. Details is an independent immutable copy.
type structuredProbeDiagnostic struct {
	Outcome                 probeOutcome
	Reason                  probeValidationReason
	Stage                   structuredStage
	AssistantError          assistantErrorKind
	StructuredOutputPresent bool
	StructuredOutputNull    bool
	SchemaChecked           bool
	SchemaValid             bool
	Details                 *StructuredDiagnostics
}

func (d structuredProbeDiagnostic) accepted() bool {
	return (d.Outcome == probeValidatedFinal || d.Outcome == probeValidatedAction) && d.Reason == probeValidationOK
}

func (d structuredProbeDiagnostic) summary() string {
	envelope := "unknown"
	if d.Outcome == probeValidatedFinal {
		envelope = "final"
	} else if d.Outcome == probeValidatedAction {
		envelope = "action"
	}
	text := fmt.Sprintf("probe_outcome=%s structured_output_present=%t structured_output_null=%t schema_checked=%t schema_valid=%t envelope_kind=%s validation_reason=%s",
		d.Outcome, d.StructuredOutputPresent, d.StructuredOutputNull, d.SchemaChecked, d.SchemaValid, envelope, d.Reason)
	e := &Error{structuredStage: d.Stage, assistantError: d.AssistantError}
	if stage := e.StructuredStage(); stage != "" {
		text += " stage=" + stage
	}
	if d.Details != nil {
		text += " details=(" + d.Details.summary() + ")"
	} else if reason := e.AssistantReason(); reason != "" {
		text += " reason=" + reason
	}
	return text
}

// The generator boundary returns only locally validated proposals on success.
// Validation of the response and the probe's stricter expected-Final assertion
// are separate: a valid Final with other prose must not masquerade as a schema
// failure. Keep that assertion without logging the prose or silently relaxing it.
func classifyStructuredProbe(p actionProposal, response llm.Response, err error) structuredProbeDiagnostic {
	d := structuredProbeDiagnostic{}
	if err != nil {
		d.Reason = probeTypedFailure
		var e *Error
		if errors.As(err, &e) {
			d.Stage, d.AssistantError = e.structuredStage, e.assistantError
			if detail := e.StructuredDetails(); detail != nil {
				d.Details = detail
				d.StructuredOutputPresent, d.StructuredOutputNull = detail.StructuredOutputPresent, detail.StructuredOutputNull
				d.SchemaChecked, d.SchemaValid = detail.SchemaChecked, detail.SchemaValid
			}
			switch d.Stage {
			case structuredResultMissingOutput:
				d.Outcome = probeStructuredMissing
			case structuredResultNullOutput:
				d.Outcome = probeStructuredNull
			case structuredSchemaRejected, structuredArgumentsSchemaRejected:
				d.Outcome = probeSchemaInvalid
			case structuredActionEnvelopeInvalid, structuredUnknownTool:
				d.Outcome = probeEnvelopeInvalid
			default:
				switch e.Code {
				case "external_reauth_required", "subscription_unavailable", "rate_limited", "invalid_model", "invalid_effort", "provider_request_rejected", "generation_output_limit", "subprocess_failure", "structured_unavailable", "structured_generation_timeout":
					d.Outcome = probeProviderFailure
				}
				if d.AssistantError != assistantErrorNone {
					d.Outcome = probeProviderFailure
				}
			}
		} else if errors.Is(err, context.DeadlineExceeded) {
			d.Outcome, d.Reason = probeProviderFailure, probeTimeout
		} else if errors.Is(err, context.Canceled) {
			d.Outcome, d.Reason = probeProviderFailure, probeCanceled
		} else {
			// Do not render arbitrary Go error strings, even in a probe.
			d.Outcome, d.Reason = probeProviderFailure, probeUnclassifiedFailure
		}
		return d
	}
	switch {
	case p.Type == "final" && p.Final != nil && p.Action == nil:
		d.Outcome = probeValidatedFinal
		if p.Final.Message != structuredProbeFinalMessage {
			d.Reason = probeFinalMessageMismatch
		}
	case p.Type == "action" && p.Action != nil && p.Final == nil:
		d.Outcome, d.Reason = probeValidatedAction, probeUnexpectedAction
	default:
		d.Outcome, d.Reason = probeEnvelopeInvalid, probeInvalidEnvelope
		return d
	}
	d.StructuredOutputPresent, d.SchemaChecked, d.SchemaValid = true, true, true
	if len(response.Output) != 0 {
		d.Reason = probeUnexpectedResponseOutput
	}
	return d
}

// Level 1 is Final-only. The unchanged schemas for Levels 2-5 allow Final or a
// validated Action. An Action is merely evidence that its schema branch worked;
// it never becomes a public ToolCall or invokes any executor. Keep exact Final
// wording and every invalid/output failure check from the common classifier.
func classifyStructuredProbeLevel(level int, p actionProposal, response llm.Response, err error) structuredProbeDiagnostic {
	d := classifyStructuredProbe(p, response, err)
	if level >= 2 && level <= 5 && d.Outcome == probeValidatedAction && d.Reason == probeUnexpectedAction {
		d.Reason = probeValidationOK
	}
	return d
}

type structuredProbeReport struct {
	Level        int
	Variant      string
	SchemaBytes  int
	SchemaSHA256 string
	Diagnostic   structuredProbeDiagnostic
}

// Each level builds and compiles a new schema, creates a new deadline, and calls
// the adapter once. Reports are inert: no previous result or diagnostic feeds
// into any request. The real adapter starts/reaps a new process for each call.
func runStructuredProbeLevels(ctx context.Context, levels int, model llm.Model, schemaFor func(int) *actionSchema, generate structuredGenerator) []structuredProbeReport {
	sequence, err := selectStructuredProbeLevels(levels, 0)
	if err != nil {
		panic("invalid offline probe level selection")
	}
	return runStructuredProbeSequence(ctx, sequence, model, schemaFor, generate)
}

func runStructuredProbeSequence(ctx context.Context, levels []int, model llm.Model, schemaFor func(int) *actionSchema, generate structuredGenerator) []structuredProbeReport {
	var reports []structuredProbeReport
	for _, level := range levels {
		invocation := newStructuredProbeInvocation(model, schemaFor(level))
		report := structuredProbeReport{Level: level, SchemaBytes: len(invocation.Schema.document), SchemaSHA256: fmt.Sprintf("%x", sha256.Sum256(invocation.Schema.document))}
		generationCtx, cancel := context.WithTimeout(ctx, invocation.Timeout)
		p, response, err := generate(generationCtx, invocation.Model, invocation.Schema, invocation.System, invocation.Input)
		cancel()
		report.Diagnostic = classifyStructuredProbeLevel(level, p, response, err)
		reports = append(reports, report)
		if !report.Diagnostic.accepted() {
			break
		}
	}
	return reports
}
