package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

const structuredDeny = bridgeBuiltinDeny + ",mcp__*"

func structuredArgs() []string {
	args := isolationArgs()
	for i, arg := range args {
		if arg == "--disallowedTools" {
			args[i+1] = structuredDeny
		}
	}
	return append(args, "--allowedTools", structuredOutputTool)
}

func validateStructuredLaunch(args []string) error {
	for _, required := range []struct{ flag, value string }{{"--allowedTools", structuredOutputTool}, {"--disallowedTools", structuredDeny}, {"--max-turns", structuredSerializerTurns}} {
		count := 0
		for i, arg := range args {
			if strings.HasPrefix(arg, required.flag+"=") {
				return &Error{Code: "isolation_contract_invalid"}
			}
			if arg == required.flag {
				count++
				if i+1 == len(args) || args[i+1] != required.value {
					return &Error{Code: "isolation_contract_invalid"}
				}
			}
		}
		if count != 1 {
			return &Error{Code: "isolation_contract_invalid"}
		}
	}
	copy := append([]string(nil), args...)
	for i, arg := range copy {
		if arg == "--disallowedTools" {
			copy[i+1] = "*"
		}
	}
	return validateLaunchContract(copy)
}

func (c *Client) validateBridgeModel(ctx context.Context, req llm.Request) error {
	catalog, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	m, err := catalog.FindSelection(req.Model.ID)
	if err != nil && !catalog.Authoritative && req.Model.ID == c.config.CurrentModel.ID && req.Model.ReasoningEffort == c.config.CurrentModel.ReasoningEffort {
		m, err = modelcatalog.Model{ID: req.Model.ID, Efforts: []llm.ReasoningEffort{req.Model.ReasoningEffort}}, nil
	}
	if err != nil {
		return &Error{Code: "invalid_model"}
	}
	if !m.AllowsEffort(req.Model.ReasoningEffort) {
		return &Error{Code: "invalid_effort"}
	}
	if req.Model.MaxOutputTokens != nil {
		return &Error{Code: "unsupported_input"}
	}
	return nil
}

func (c *Client) probeStructured(ctx context.Context, tools []llm.Tool) error {
	schema, err := newActionSchema(tools)
	if err != nil {
		return err
	}
	path, env, err := c.prepare(ctx)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _, err = c.structuredGeneration(probeCtx, path, env, llm.Model{}, schema, "", "", true)
	return err
}

func (c *Client) respondStructured(ctx context.Context, req llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	if opt.Tools == nil || opt.RefreshContext == nil {
		return llm.Response{}, &Error{Code: "bridge_host_required"}
	}
	if err := c.validateBridgeModel(ctx, req); err != nil {
		return llm.Response{}, err
	}
	schema, err := newActionSchema(req.Tools)
	if err != nil {
		return llm.Response{}, err
	}
	path, env, err := c.prepare(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	if opt.BeginTools != nil {
		if err := opt.BeginTools(ctx); err != nil {
			return llm.Response{}, safeBridgeCallbackError(err)
		}
	}
	return runStructuredRounds(ctx, req, opt, schema, c.config.ToolBridge.limits(), func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
		return c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
	})
}

// The round controller owns no process or executor. Generations return validated
// proposals; the Host callbacks own context, permissions, Operations and receipts.
// Keeping this boundary explicit allows every round/replay path to be tested
// without launching even a fake CLI.
type structuredGenerator func(context.Context, llm.Model, *actionSchema, string, string) (actionProposal, llm.Response, error)

func runStructuredRounds(ctx context.Context, req llm.Request, opt llm.RequestOptions, schema *actionSchema, limits ToolBridgeConfig, generate structuredGenerator) (llm.Response, error) {
	if opt.Tools == nil || opt.RefreshContext == nil || generate == nil {
		return llm.Response{}, &Error{Code: "bridge_host_required"}
	}
	model := req.Model
	if opt.InputBudget == 0 {
		opt.InputBudget = 24576
	}
	scope := opt.ActionScope
	if scope == "" {
		scope = uuid.New().String()
	}
	scopeHash := sha256.Sum256([]byte(scope))
	prefix := "structured-" + hex.EncodeToString(scopeHash[:16]) + "-"
	seen := map[string]struct {
		fingerprint string
		outcome     llm.ToolOutcome
	}{}
	var last *llm.ToolOutcome
	actions := 0
	var replayUsage []llm.Usage
	// Replayed proposals also consume a generation bound, preventing a model
	// from looping indefinitely without creating new Operations.
	for generation := 0; generation <= limits.MaxActionRounds; generation++ {
		if err := ctx.Err(); err != nil {
			return llm.Response{}, err
		}
		resultLimit := min(limits.ResultBytes, int(opt.InputBudget/4))
		if resultLimit < 512 {
			return llm.Response{}, &Error{Code: "bridge_context_budget"}
		}
		reserve, err := structuredTransportReserve(last, resultLimit)
		if err != nil {
			return llm.Response{}, err
		}
		reserve += contextengine.Estimate(schema.contract)
		refreshed, budget, err := opt.RefreshContext(ctx, reserve)
		if err != nil {
			return llm.Response{}, safeBridgeCallbackError(err)
		}
		if refreshed.Model != model {
			return llm.Response{}, &Error{Code: "structured_runtime_changed"}
		}
		updated, err := newActionSchema(refreshed.Tools)
		if err != nil {
			return llm.Response{}, err
		}
		if !sameSchema(schema, updated) {
			return llm.Response{}, &Error{Code: "structured_runtime_changed"}
		}
		req = refreshed
		if budget > 0 {
			opt.InputBudget = budget
		}
		system, input, err := structuredPrompt(req.Input, last, resultLimit)
		if err != nil {
			return llm.Response{}, err
		}
		system += schema.contract
		system += fmt.Sprintf("\nUnreal structured generation: %d. Maximum actions: %d.", generation, limits.MaxActionRounds)
		if contextengine.Estimate(system)+contextengine.Estimate(input) > opt.InputBudget {
			return llm.Response{}, &Error{Code: "bridge_context_budget"}
		}
		generationCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.TimeoutMillis)*time.Millisecond)
		proposal, response, err := generate(generationCtx, model, schema, system, input)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return llm.Response{}, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return llm.Response{}, &Error{Code: "structured_generation_timeout"}
			}
			return llm.Response{}, err
		}
		response.ID = uuid.New().String()
		if proposal.Type == "final" {
			if err := addReplayUsage(&response.Usage, replayUsage); err != nil {
				return llm.Response{}, err
			}
			response.Output = []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: proposal.Final.Message}}}
			return response, nil
		}
		if generation == limits.MaxActionRounds || actions >= limits.MaxActionRounds {
			return llm.Response{}, &Error{Code: "action_round_limit"}
		}
		fingerprint, err := actionFingerprint(proposal)
		if err != nil {
			return llm.Response{}, err
		}
		id := prefix + proposal.Action.ID
		if prior, ok := seen[id]; ok {
			if prior.fingerprint != fingerprint {
				return llm.Response{}, &Error{Code: "bridge_duplicate_conflict"}
			}
			copy := prior.outcome
			last = &copy
			replayUsage = append(replayUsage, response.Usage)
		} else {
			if err := addReplayUsage(&response.Usage, replayUsage); err != nil {
				return llm.Response{}, err
			}
			replayUsage = nil
			call := llm.ToolCall{CallID: id, Name: proposal.Action.Tool, Arguments: strings.SplitN(fingerprint, "\n", 2)[1]}
			response.Output = []llm.Item{{Type: llm.ItemToolCall, Data: call}}
			toolCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.TimeoutMillis)*time.Millisecond)
			outcomes, err := opt.Tools(toolCtx, response)
			cancel()
			if err != nil {
				return llm.Response{}, safeBridgeCallbackError(err)
			}
			if len(outcomes) != 1 || outcomes[0].Result.CallID != id {
				return llm.Response{}, &Error{Code: "bridge_receipt_invalid"}
			}
			seen[id] = struct {
				fingerprint string
				outcome     llm.ToolOutcome
			}{fingerprint, outcomes[0]}
			copy := outcomes[0]
			last = &copy
			actions++
		}
	}
	return llm.Response{}, &Error{Code: "action_round_limit"}
}

func structuredTransportReserve(last *llm.ToolOutcome, limit int) (int64, error) {
	// Estimate only private protocol framing and the bounded latest receipt.
	// This empty input is never sent or added to canonical history.
	framing := []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser}}}
	system, input, err := structuredPrompt(framing, last, limit)
	return contextengine.Estimate(system) + contextengine.Estimate(input) + 256, err
}

func addReplayUsage(target *llm.Usage, previous []llm.Usage) error {
	for _, p := range previous {
		for _, field := range []struct {
			value *int64
			prior int64
		}{{&target.InputTokens, p.InputTokens}, {&target.CachedInputTokens, p.CachedInputTokens}, {&target.CacheWriteInputTokens, p.CacheWriteInputTokens}, {&target.OutputTokens, p.OutputTokens}, {&target.ReasoningTokens, p.ReasoningTokens}} {
			if *field.value > math.MaxInt64-field.prior {
				return structuredError("structured_protocol_invalid", structuredUsageInvalid)
			}
			*field.value += field.prior
		}
		for _, unknown := range p.Unknown {
			if !slices.Contains(target.Unknown, unknown) {
				target.Unknown = append(target.Unknown, unknown)
			}
		}
	}
	return nil
}

func sameSchema(a, b *actionSchema) bool {
	return bytes.Equal(a.document, b.document) && bytes.Equal(a.requestDocument(), b.requestDocument())
}
func sensitiveReceipt(text string) bool { return secretguard.Sensitive(text) }

// One generation = one independent isolated process. SDK controls and stdout
// live only in this stack frame. No session/replay/private continuation state
// is reused, and cleanup reaps the entire process group on every exit path.
func (c *Client) structuredGeneration(ctx context.Context, path string, env []string, model llm.Model, schema *actionSchema, system, input string, probe bool) (actionProposal, llm.Response, error) {
	dir, err := os.MkdirTemp("", "unreal-claude-structured-")
	if err != nil {
		return actionProposal{}, llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer os.RemoveAll(dir)
	systemPath := filepath.Join(dir, "system.txt")
	if err = os.WriteFile(systemPath, []byte(system), 0600); err != nil {
		return actionProposal{}, llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	request, err := buildStructuredRequest(model, env, schema, systemPath, input, probe)
	if err != nil {
		return actionProposal{}, llm.Response{}, err
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, path, request.Arguments...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, request.Environment, io.Discard
	if err = isolateProcess(cmd); err != nil {
		return actionProposal{}, llm.Response{}, err
	}
	reader, output, err := os.Pipe()
	if err != nil {
		return actionProposal{}, llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer reader.Close()
	defer output.Close()
	stdin, writer, err := os.Pipe()
	if err != nil {
		return actionProposal{}, llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	defer stdin.Close()
	defer writer.Close()
	cmd.Stdin, cmd.Stdout = stdin, output
	if err = cmd.Start(); err != nil {
		return actionProposal{}, llm.Response{}, &Error{Code: "subprocess_failure"}
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
	proposal, response, streamErr := runStructuredProtocol(reader, writer, schema, input, probe)
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
		return actionProposal{}, llm.Response{}, ctx.Err()
	}
	if streamErr != nil {
		return actionProposal{}, llm.Response{}, streamErr
	}
	if natural && waitErr != nil {
		return actionProposal{}, llm.Response{}, &Error{Code: "subprocess_failure"}
	}
	return proposal, response, nil
}
