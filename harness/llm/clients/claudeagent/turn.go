package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

const (
	gateHook  = "unreal-gate"
	batchHook = "unreal-batch"
	// ranPrefix starts the note Claude reads after Unreal handled a command
	// that Claude Code declined (U22).
	ranPrefix = "Unreal Agent handled"
)

// turn is one Claude Code process answering one Unreal model request.
type turn struct {
	ctx       context.Context
	cancel    context.CancelFunc
	opt       llm.RequestOptions
	out       *writer
	results   *observed.Results
	approver  *approver
	directory string
	session   string
	echoed    bool // Claude Code took up Unreal's message
	subagents SubagentCLI

	usageLimit subscriptionLimit // observed and read by the stream reader

	mu               sync.Mutex
	model            string
	text             []llm.Item                 // main-agent text not yet recorded
	refused          map[string]bool            // shell calls Claude Code itself refused
	denied           map[string]bool            // calls Unreal itself denied
	reported         map[string]observed.Result // tool results the model read
	seq              int
	failure          error
	batchInputTokens int64 // latest main-call input, independent of total turn usage
	windowStopped    bool  // Unreal answered a tool batch with continue:false
	// The model steps since the last recorded response: their usage and the
	// time spent streaming them, so TPS never counts tool or approval waits.
	steps               stepStats
	measureFirstRequest bool
	firstRequest        *llm.Usage
	stepStart           time.Time
	stepUsage           usage
	usageReported       bool
	messages            map[string]message
	toolStarts          map[string]time.Time
	nativeTools         map[string]nativeToolActivity
	// mark is when the next model request can start: Claude Code ready, the
	// previous step done, or a tool batch answered. A step's time runs from
	// there, so it includes the wait for the first token as Codex's does.
	mark    time.Time
	startup startupTiming
	streams *streamActivities

	record  sync.Mutex // one rendezvous round at a time
	workers sync.WaitGroup
	now     func() time.Time
}

type stepStats struct {
	known                     bool
	input, cached, cacheWrite int64
	contextTokens             int64
	contextKnown              bool
	output                    int64
	reasoning                 int64
	reasoningUnknown          bool
	micros                    int64
}

func newTurn(ctx context.Context, cancel context.CancelFunc, opt llm.RequestOptions, out *writer, results *observed.Results, a *approver, directory, session string) *turn {
	return &turn{ctx: ctx, cancel: cancel, opt: opt, out: out, results: results, approver: a, directory: directory, session: session, refused: map[string]bool{}, denied: map[string]bool{}, reported: map[string]observed.Result{}, toolStarts: map[string]time.Time{}, now: time.Now}
}

func (t *turn) recordSubscriptionUsage(f frame, model llm.Model) {
	info := f.RateLimitInfo
	u := llm.SubscriptionUsage{}
	switch info.Window {
	case "five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "overage":
		u.Window = info.Window
	}
	switch info.Status {
	case "allowed", "allowed_warning", "rejected":
		u.Status = info.Status
		if f.ParentToolUseID == nil {
			t.usageLimit.rate = info
		}
	}
	if info.Utilization != nil && *info.Utilization >= 0 {
		u.UsedPercent = new(100 * *info.Utilization)
	}
	if info.ResetsAt > 0 {
		u.ResetsAt = time.Unix(info.ResetsAt, 0).UTC()
	}
	if u.Status != "" || u.UsedPercent != nil || !u.ResetsAt.IsZero() {
		t.mu.Lock()
		actual := t.model
		t.mu.Unlock()
		if actual == "" {
			actual = model.ID
		}
		t.recordActivity(llm.ActivityRecord{Kind: llm.ActivitySubscriptionUsage, Provider: ProviderID,
			ID: t.opt.Identity.TurnID, Model: actual, Effort: string(model.ReasoningEffort), Subscription: []llm.SubscriptionUsage{u}})
	}
}

// handle acts on one frame. It returns the result frame when the turn ends.
func (t *turn) handle(f frame) *frame {
	t.recordStartup(f)
	switch f.Type {
	case "system":
		switch f.Subtype {
		case "init":
			t.mu.Lock()
			t.model = f.Model
			t.mark = t.now()
			t.mu.Unlock()
		case "permission_denied":
			// Claude Code refused a shell command on its own: a deny rule on the
			// whole command ("rule") or on one of its parts ("subcommandResults"),
			// or another of its checks. Refusals Unreal made itself (its gate or
			// its approval answer) are not handed back to Unreal's shell.
			if (f.ToolName == "Bash" || f.ToolName == "PowerShell") && f.ToolUseID != "" && f.DecisionReasonType != "permissionPromptTool" {
				t.mu.Lock()
				t.refused[f.ToolUseID] = true
				t.mu.Unlock()
			}
		}
	case "stream_event":
		var event streamEvent
		if f.ParentToolUseID != nil || json.Unmarshal(f.Event, &event) != nil {
			return nil
		}
		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "text_delta" && t.opt.Progress != nil {
				t.opt.Progress(llm.Progress{Attempt: 1, Delta: event.Delta.Text})
			}
		case "message_start":
			t.mu.Lock()
			t.captureFirstRequest(event.Message.Usage)
			t.stepStart, t.stepUsage = t.mark, usage{}
			t.batchInputTokens = 0
			if t.stepStart.IsZero() {
				t.stepStart = t.now()
			}
			if u := event.Message.Usage; u != nil {
				t.stepUsage = *u
				t.steps.contextTokens, t.steps.contextKnown = u.inputTokens(), true
				t.batchInputTokens = t.steps.contextTokens
			}
			t.mu.Unlock()
		case "message_delta":
			if u := event.Usage; u != nil {
				t.mu.Lock()
				t.stepUsage.OutputTokens = u.OutputTokens
				if u.ReasoningTokens != nil {
					t.stepUsage.ReasoningTokens = u.ReasoningTokens
				}
				if u.ThinkingTokens != nil {
					t.stepUsage.ThinkingTokens = u.ThinkingTokens
				}
				t.mu.Unlock()
			}
		case "message_stop":
			t.mu.Lock()
			if !t.stepStart.IsZero() {
				u := t.stepUsage
				read, write := u.cacheTokens()
				t.steps.known = true
				t.steps.input += u.InputTokens + read + write
				t.steps.cached += read
				t.steps.cacheWrite += write
				t.steps.output += u.OutputTokens
				if reasoning, known := u.reasoningTokens(); known {
					t.steps.reasoning += reasoning
				} else {
					t.steps.reasoningUnknown = true
				}
				now := t.now()
				t.steps.micros += now.Sub(t.stepStart).Microseconds()
				t.stepStart, t.mark = time.Time{}, now
			}
			t.mu.Unlock()
		}
	case "assistant":
		var incoming struct {
			message
			StopReason string `json:"stop_reason"`
		}
		if json.Unmarshal(f.Message, &incoming) != nil {
			return nil
		}
		m := incoming.message
		t.usageLimit.observeAssistant(f, m)
		t.mu.Lock()
		if f.ParentToolUseID == nil {
			t.captureFirstRequest(m.Usage)
			if m.Usage != nil {
				if inputTokens := m.Usage.inputTokens(); inputTokens > 0 {
					t.batchInputTokens = inputTokens
				}
			}
		}
		limited := llm.IsOutputLimit(incoming.StopReason)
		if m.ID != "" && m.Usage != nil {
			if t.messages == nil {
				t.messages = map[string]message{}
			}
			t.messages[m.ID] = message{Model: m.Model, Usage: m.Usage}
		}
		if f.ParentToolUseID != nil {
			// The parent Agent tool call is the link to this native child's
			// transcript. These are observations, never executable tool calls.
			t.seq++
			publicMessage, _ := json.Marshal(m)
			t.text = append(t.text, llm.Item{ProviderID: itemPrefix + m.ID + "/native/" + strconv.Itoa(t.seq), Type: llm.ItemMessage, OutputLimit: limited, Data: llm.Message{Role: llm.RoleAssistant, Text: "[Claude Code Agent " + *f.ParentToolUseID + "] " + string(publicMessage)}})
		}
		t.mu.Unlock()
		for _, b := range m.Content {
			if b.Type == "tool_use" && f.ParentToolUseID == nil && b.Name != "Bash" {
				t.startNativeTool(b.ID, b.Name)
			}
			switch {
			case b.Type == "text" && f.ParentToolUseID == nil && strings.TrimSpace(b.Text) != "":
				t.mu.Lock()
				t.seq++
				t.text = append(t.text, llm.Item{ProviderID: itemPrefix + m.ID + "/" + strconv.Itoa(t.seq), Type: llm.ItemMessage, OutputLimit: limited, Data: llm.Message{Role: llm.RoleAssistant, Text: b.Text}})
				t.mu.Unlock()
			case b.Type == "tool_use" && f.ParentToolUseID == nil && t.opt.Progress != nil:
				t.opt.Progress(llm.Progress{Attempt: 1, Delta: "\n› " + t.callSubject(b.Name, b.Input) + "\n"})
			case b.Type == "tool_use" && f.ParentToolUseID != nil && t.opt.Progress != nil:
				// A sub-agent's own tool calls, indented under the Agent call
				// that started them. Its text stays in the recorded observations.
				t.opt.Progress(llm.Progress{Attempt: 1, Delta: "\n" + subagentLinePrefix + t.callSubject(b.Name, b.Input) + "\n"})
			}
		}
	case "user":
		if f.IsReplay {
			t.mu.Lock()
			t.echoed = true
			t.mu.Unlock()
		}
		var m message
		if json.Unmarshal(f.Message, &m) != nil {
			return nil
		}
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				t.endNativeBash(b.ToolUseID, "")
				t.endNativeTool(b.ToolUseID, "")
				t.mu.Lock()
				t.reported[b.ToolUseID] = observed.Result{Content: contentText(b.Content), IsError: b.IsError}
				t.mu.Unlock()
			}
		}
	case "control_request":
		var req controlRequest
		if json.Unmarshal(f.Request, &req) != nil {
			_ = t.out.fail(f.RequestID, "malformed request")
			return nil
		}
		id := f.RequestID
		switch req.Subtype {
		case "can_use_tool":
			t.workers.Go(func() { t.approve(id, req) })
		case "hook_callback":
			var in hookInput
			_ = json.Unmarshal(req.Input, &in)
			t.workers.Go(func() { t.hook(id, req.CallbackID, in) })
		default:
			_ = t.out.fail(id, "unsupported request: "+req.Subtype)
		}
	case "result":
		// Before Unreal's message is taken up, a result ends work Claude Code
		// queued itself, such as a notice about a background command that an
		// interrupted run left behind; the answer to Unreal's message follows.
		t.mu.Lock()
		echoed := t.echoed
		t.mu.Unlock()
		if !echoed && !f.IsError {
			return nil
		}
		return &f
	}
	return nil
}

// callSubject names one tool call in the progress stream. Claude's Agent calls
// show the model and the task instead of their JSON input; every other call
// keeps approvalSubject's text.
func (t *turn) callSubject(name string, input jsontext.Value) string {
	if name == "Agent" {
		return agentCallSubject(input, t.subagents.agentModel)
	}
	_, scope := approvalSubject(name, input)
	return scope
}

func (t *turn) approve(id string, req controlRequest) {
	allowed, reason := t.approver.decide(t.ctx, req.ToolName, req.Input, req.ToolUseID)
	if !allowed {
		t.markDenied(req.ToolUseID)
	}
	if allowed {
		input := req.Input
		if len(input) == 0 {
			input = jsontext.Value("{}")
		}
		_ = t.out.reply(id, map[string]any{"behavior": "allow", "updatedInput": input})
		return
	}
	_ = t.out.reply(id, map[string]any{"behavior": "deny", "message": reason})
}

func (t *turn) hook(id, callback string, in hookInput) {
	switch callback {
	case gateHook:
		if in.ToolName == "Bash" && subagentWait(inputFields(in.ToolInput).Command) && t.opt.BeginTools != nil {
			if err := t.opt.BeginTools(t.ctx, true); err != nil {
				t.fail(err)
				return
			}
		}
		answer := t.approver.gate(t.ctx, in)
		decision, _ := answer["hookSpecificOutput"].(map[string]any)
		if decision["permissionDecision"] == "deny" {
			t.markDenied(in.ToolUseID)
		} else if in.ToolName == "Bash" {
			t.startNativeBash(in.ToolUseID)
		}
		_ = t.out.reply(id, answer)
	case batchHook:
		if err := t.out.reply(id, t.batch(in)); err != nil {
			t.fail(err)
		}
	default:
		_ = t.out.reply(id, map[string]any{})
	}
}

// batch records a resolved batch of Claude Code tool calls in Unreal's history.
// A command Claude Code refused on its own (U22) is handed to Unreal's own
// Bash instead (U22), and its result goes back to Claude as additional context.
func (t *turn) batch(in hookInput) (answer map[string]any) {
	t.record.Lock()
	defer t.record.Unlock()
	var recordedInputTokens int64
	// Even an empty batch can carry inbox inputs. Preserve any U22 result in
	// the same additionalContext and acknowledge input only through the Host.
	defer func() {
		if t.ctx.Err() != nil {
			return
		}
		var batch llm.ToolBatchContinuation
		var err error
		if t.opt.AfterToolBatch != nil {
			batch, err = t.opt.AfterToolBatch(t.ctx, recordedInputTokens)
		} else if t.opt.Steering != nil {
			batch.Inputs, err = t.opt.Steering(t.ctx)
		}
		if err != nil {
			t.fail(err)
			return
		}
		if batch.RotateWindow {
			t.mu.Lock()
			t.windowStopped = true
			t.mu.Unlock()
			// The top-level fields stop the agentic loop before the next call.
			answer = map[string]any{"continue": false, "stopReason": "Unreal Agent is continuing in a fresh context window."}
			return
		}
		inputs := batch.Inputs
		if len(inputs) == 0 {
			return
		}
		if answer == nil {
			answer = map[string]any{}
		}
		output, _ := answer["hookSpecificOutput"].(map[string]any)
		if output == nil {
			output = map[string]any{"hookEventName": "PostToolBatch"}
			answer["hookSpecificOutput"] = output
		}
		var notes []string
		if prior, _ := output["additionalContext"].(string); prior != "" {
			notes = append(notes, prior)
		}
		for _, input := range inputs {
			if input.Peer {
				notes = append(notes, input.Text)
			} else {
				notes = append(notes, fmt.Sprintf("The user wrote while you were working: %q. Answer the user first, then continue if still appropriate.", input.Text))
			}
		}
		output["additionalContext"] = strings.Join(notes, "\n\n")
	}()
	defer func() {
		if t.opt.BeginTools != nil && t.ctx.Err() == nil {
			if err := t.opt.BeginTools(t.ctx, false); err != nil {
				t.fail(err)
			}
		}
	}()
	if t.ctx.Err() != nil {
		return map[string]any{}
	}
	// A policy refusal can reach PostToolBatch without PreToolUse. Its wait
	// runs through Unreal's Bash and must yield to the user as well.
	for _, call := range in.ToolCalls {
		if call.ToolName == "Bash" && subagentWait(inputFields(call.ToolInput).Command) && t.opt.BeginTools != nil {
			if err := t.opt.BeginTools(t.ctx, true); err != nil {
				t.fail(err)
				return map[string]any{}
			}
			break
		}
	}
	t.mu.Lock()
	output, model := t.takeOutput(), t.model
	steps := t.takeSteps()
	var inputTokens int64
	if in.AgentID == "" {
		inputTokens, t.batchInputTokens = t.batchInputTokens, 0
	}
	t.mu.Unlock()
	type handed struct{ id, command string }
	var executed []handed
	var observedIDs []string
	for _, call := range in.ToolCalls {
		if call.ToolUseID == "" {
			continue
		}
		if call.ToolName == "Bash" {
			t.endNativeBash(call.ToolUseID, "post_tool_batch")
		}
		t.endNativeTool(call.ToolUseID, "post_tool_batch")
		t.mu.Lock()
		refused := t.refused[call.ToolUseID] && !t.denied[call.ToolUseID]
		result, reported := t.reported[call.ToolUseID]
		t.mu.Unlock()
		if command := inputFields(call.ToolInput).Command; refused && command != "" {
			arguments, _ := json.Marshal(map[string]string{"command": inDirectory(in.Cwd, t.directory, command)})
			output = append(output, llm.Item{ProviderID: itemPrefix + call.ToolUseID, Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: call.ToolUseID, Name: tool.BashName, Arguments: string(arguments)}})
			executed = append(executed, handed{call.ToolUseID, command})
			continue
		}
		if !reported {
			result = observed.Result{Content: contentText(call.ToolResponse)}
		}
		if !result.IsError && (reported || call.ToolResponse.Kind() != jsontext.KindNull && len(call.ToolResponse) != 0) {
			switch call.ToolName {
			case "Write", "Edit", "MultiEdit", "NotebookEdit":
				fields := inputFields(call.ToolInput)
				if path := first(fields.FilePath, fields.NotebookPath); path != "" {
					result.WrittenPaths = []string{path}
				}
			}
		}
		t.results.Put(call.ToolUseID, result)
		observedIDs = append(observedIDs, call.ToolUseID)
		arguments, _ := json.Marshal(observed.Arguments{Tool: call.ToolName, Input: call.ToolInput})
		output = append(output, llm.Item{ProviderID: itemPrefix + call.ToolUseID, Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: call.ToolUseID, Name: tool.ClaudeCodeName, Arguments: string(arguments)}})
	}
	if len(executed)+len(observedIDs) == 0 {
		t.mu.Lock()
		t.text = append(output, t.text...)
		t.mu.Unlock()
		return map[string]any{}
	}
	usage, generation := steps.report(model)
	t.mu.Lock()
	t.usageReported = true
	first := t.firstRequestUsage()
	t.mu.Unlock()
	outcomes, err := t.opt.Tools(t.ctx, llm.Response{ID: uuid.New().String(), Model: model, Stop: llm.StopComplete, Output: output, Usage: usage, Generation: generation, FirstRequestUsage: first})
	t.mu.Lock()
	t.mark = t.now() // tool runs and approvals are never generation time
	t.mu.Unlock()
	if err != nil {
		t.results.Drop(observedIDs...)
		t.fail(fmt.Errorf("record Claude Code tool calls: %w", err))
		return map[string]any{}
	}
	recordedInputTokens = inputTokens
	byID := map[string]llm.ToolOutcome{}
	for _, outcome := range outcomes {
		byID[outcome.Result.CallID] = outcome
	}
	var notes []string
	for _, h := range executed {
		var parts []string
		for _, output := range byID[h.id].Result.Output {
			if output.Kind == llm.ToolResultText {
				parts = append(parts, output.Value)
			}
		}
		result := strings.Join(parts, "\n")
		if result == "" {
			result = "(no output)"
		}
		notes = append(notes, fmt.Sprintf("%s `%s` in its own shell, because Claude Code refused it. Unreal Agent's result (approval and execution):\n%s", ranPrefix, h.command, result))
	}
	if len(notes) == 0 {
		return map[string]any{}
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolBatch", "additionalContext": strings.Join(notes, "\n\n")}}
}

// markDenied notes a call Unreal denied, before Claude Code reports it.
func (t *turn) markDenied(toolUseID string) {
	if toolUseID == "" {
		return
	}
	t.mu.Lock()
	t.denied[toolUseID] = true
	t.mu.Unlock()
}

// inDirectory runs a refused command where Claude Code's shell was, not where
// Unreal's Bash starts.
func inDirectory(cwd, workspace, command string) string {
	if cwd == "" || cwd == workspace {
		return command
	}
	return "cd '" + strings.ReplaceAll(cwd, "'", `'\''`) + "' && " + command
}

func (t *turn) fail(err error) {
	t.mu.Lock()
	if t.failure == nil {
		t.failure = err
	}
	t.mu.Unlock()
	t.cancel()
}

func (t *turn) failed() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failure
}

// finish returns the text after the last recorded batch as the final response.
func (t *turn) finish(result *frame) llm.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	output := t.takeOutput()
	if len(output) == 0 && strings.TrimSpace(result.Result) != "" {
		output = []llm.Item{{ProviderID: itemPrefix + uuid.New().String(), Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: result.Result}}}
	}
	response := llm.Response{ID: uuid.New().String(), Model: t.model, Stop: llm.StopComplete, Output: output, FirstRequestUsage: t.firstRequestUsage()}
	response.Usage, response.Generation = t.takeSteps().report(t.model)
	if !t.usageReported && result.Usage != nil {
		// A result may carry usage even if the CLI omitted stream events.
		if response.Generation == nil {
			response.Usage = reportedUsage(*result.Usage)
		}
		if reasoning, known := result.Usage.reasoningTokens(); known {
			response.Usage.ReasoningTokens = reasoning
			response.Usage.Unknown = slices.DeleteFunc(response.Usage.Unknown, func(f llm.UsageField) bool { return f == llm.UsageReasoning })
		}
	}
	if response.Generation != nil {
		response.Generation.Tokens = llm.ReportedTokens(response.Usage)
	}
	if result.StopReason == "refusal" {
		response.Stop = llm.StopRefused
	}
	return response
}

// takeOutput is called with t.mu held.
func (t *turn) takeOutput() []llm.Item {
	output := t.text
	t.text = nil
	if t.streams != nil {
		t.streams.markLimitedOutput(output)
	}
	return output
}

// takeSteps hands over the steps since the last recorded response. t.mu held.
func (t *turn) takeSteps() stepStats {
	steps := t.steps
	t.steps = stepStats{}
	return steps
}

// report is the usage of the recorded steps and their own output rate. Each
// recorded response carries only its steps, so the turn's total is the sum.
func (s stepStats) report(model string) (llm.Usage, *llm.GenerationStatistics) {
	if !s.known {
		return unknownUsage(), nil
	}
	u := llm.Usage{InputTokens: s.input, CachedInputTokens: s.cached, CacheWriteInputTokens: s.cacheWrite, OutputTokens: s.output, ReasoningTokens: s.reasoning}
	if s.reasoningUnknown {
		u.Unknown = []llm.UsageField{llm.UsageReasoning}
	}
	if s.micros <= 0 {
		return u, nil
	}
	return u, &llm.GenerationStatistics{Model: model, Tokens: llm.ReportedTokens(u), ContextTokens: s.contextTokens, ContextKnown: s.contextKnown, DurationMicros: s.micros, Source: "client_clock"}
}

func unknownUsage() llm.Usage {
	return llm.Usage{Unknown: []llm.UsageField{llm.UsageInput, llm.UsageCachedInput, llm.UsageCacheWriteInput, llm.UsageOutput, llm.UsageReasoning}}
}

func reportedUsage(u usage) llm.Usage {
	read, write := u.cacheTokens()
	r := llm.Usage{InputTokens: u.InputTokens + read + write, CachedInputTokens: read, CacheWriteInputTokens: write, OutputTokens: u.OutputTokens}
	if reasoning, known := u.reasoningTokens(); known {
		r.ReasoningTokens = reasoning
	} else {
		r.Unknown = []llm.UsageField{llm.UsageReasoning}
	}
	return r
}

// captureFirstRequest runs under t.mu on the first main model call, before
// any following tool round or native child can contribute usage.
func (t *turn) captureFirstRequest(raw *usage) {
	if !t.measureFirstRequest || t.firstRequest != nil {
		return
	}
	u := unknownUsage()
	if raw != nil {
		u = reportedUsage(*raw)
		if raw.CacheReadInputTokens == nil {
			u.Unknown = append(u.Unknown, llm.UsageCachedInput)
		}
		if raw.CacheCreationInputTokens == nil {
			u.Unknown = append(u.Unknown, llm.UsageCacheWriteInput)
		}
	}
	t.firstRequest = &u
}

func (t *turn) firstRequestUsage() *llm.Usage {
	t.captureFirstRequest(nil)
	return t.firstRequest
}
