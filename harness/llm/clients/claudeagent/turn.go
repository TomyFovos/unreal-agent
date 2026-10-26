package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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
	// ranPrefix starts the note Claude reads after Unreal ran a command that
	// Claude Code refused, for example by its organization policy (U22).
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

	mu       sync.Mutex
	model    string
	text     []llm.Item                 // main-agent text not yet recorded
	refused  map[string]bool            // shell calls Claude Code itself refused
	denied   map[string]bool            // calls Unreal itself denied
	reported map[string]observed.Result // tool results the model read
	seq      int
	failure  error
	// The model steps since the last recorded response: their usage and the
	// time spent streaming them, so TPS never counts tool or approval waits.
	steps     stepStats
	stepStart time.Time
	stepUsage usage
	// mark is when the next model request can start: Claude Code ready, the
	// previous step done, or a tool batch answered. A step's time runs from
	// there, so it includes the wait for the first token as Codex's does.
	mark time.Time

	record  sync.Mutex // one rendezvous round at a time
	workers sync.WaitGroup
	now     func() time.Time
}

type stepStats struct {
	known                     bool
	input, cached, cacheWrite int64
	output                    int64
	micros                    int64
}

func newTurn(ctx context.Context, cancel context.CancelFunc, opt llm.RequestOptions, out *writer, results *observed.Results, a *approver, directory, session string) *turn {
	return &turn{ctx: ctx, cancel: cancel, opt: opt, out: out, results: results, approver: a, directory: directory, session: session, refused: map[string]bool{}, denied: map[string]bool{}, reported: map[string]observed.Result{}, now: time.Now}
}

// handle acts on one frame. It returns the result frame when the turn ends.
func (t *turn) handle(f frame) *frame {
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
			t.stepStart, t.stepUsage = t.mark, usage{}
			if t.stepStart.IsZero() {
				t.stepStart = t.now()
			}
			if u := event.Message.Usage; u != nil {
				t.stepUsage = *u
			}
			t.mu.Unlock()
		case "message_delta":
			if u := event.Usage; u != nil {
				t.mu.Lock()
				t.stepUsage.OutputTokens = u.OutputTokens
				t.mu.Unlock()
			}
		case "message_stop":
			t.mu.Lock()
			if !t.stepStart.IsZero() {
				u := t.stepUsage
				t.steps.known = true
				t.steps.input += u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				t.steps.cached += u.CacheReadInputTokens
				t.steps.cacheWrite += u.CacheCreationInputTokens
				t.steps.output += u.OutputTokens
				now := t.now()
				t.steps.micros += now.Sub(t.stepStart).Microseconds()
				t.stepStart, t.mark = time.Time{}, now
			}
			t.mu.Unlock()
		}
	case "assistant":
		var m message
		if json.Unmarshal(f.Message, &m) != nil {
			return nil
		}
		for _, b := range m.Content {
			switch {
			case b.Type == "text" && f.ParentToolUseID == nil && strings.TrimSpace(b.Text) != "":
				t.mu.Lock()
				t.seq++
				t.text = append(t.text, llm.Item{ProviderID: itemPrefix + m.ID + "/" + strconv.Itoa(t.seq), Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: b.Text}})
				t.mu.Unlock()
			case b.Type == "tool_use" && f.ParentToolUseID == nil && t.opt.Progress != nil:
				_, scope := approvalSubject(b.Name, b.Input)
				t.opt.Progress(llm.Progress{Attempt: 1, Delta: "\n› " + scope + "\n"})
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
		answer := t.approver.gate(t.ctx, in)
		if len(answer) > 0 {
			t.markDenied(in.ToolUseID)
		}
		_ = t.out.reply(id, answer)
	case batchHook:
		_ = t.out.reply(id, t.batch(in))
	default:
		_ = t.out.reply(id, map[string]any{})
	}
}

// batch records a resolved batch of Claude Code tool calls in Unreal's history.
// A command Claude Code refused on its own (U22) is handed to Unreal's own
// Bash instead (U22), and its result goes back to Claude as additional context.
func (t *turn) batch(in hookInput) map[string]any {
	t.record.Lock()
	defer t.record.Unlock()
	if t.ctx.Err() != nil {
		return map[string]any{}
	}
	t.mu.Lock()
	output, model := t.text, t.model
	t.text = nil
	steps := t.takeSteps()
	t.mu.Unlock()
	type handed struct{ id, command string }
	var executed []handed
	var observedIDs []string
	for _, call := range in.ToolCalls {
		if call.ToolUseID == "" {
			continue
		}
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
	outcomes, err := t.opt.Tools(t.ctx, llm.Response{ID: uuid.New().String(), Model: model, Stop: llm.StopComplete, Output: output, Usage: usage, Generation: generation})
	t.mu.Lock()
	t.mark = t.now() // tool runs and approvals are never generation time
	t.mu.Unlock()
	if err != nil {
		t.results.Drop(observedIDs...)
		t.fail(fmt.Errorf("record Claude Code tool calls: %w", err))
		return map[string]any{}
	}
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
	output := t.text
	t.text = nil
	if len(output) == 0 && strings.TrimSpace(result.Result) != "" {
		output = []llm.Item{{ProviderID: itemPrefix + uuid.New().String(), Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: result.Result}}}
	}
	response := llm.Response{ID: uuid.New().String(), Model: t.model, Stop: llm.StopComplete, Output: output}
	response.Usage, response.Generation = t.takeSteps().report(t.model)
	if result.StopReason == "refusal" {
		response.Stop = llm.StopRefused
	}
	return response
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
	u := llm.Usage{InputTokens: s.input, CachedInputTokens: s.cached, CacheWriteInputTokens: s.cacheWrite, OutputTokens: s.output, Unknown: []llm.UsageField{llm.UsageReasoning}}
	if s.micros <= 0 {
		return u, nil
	}
	return u, &llm.GenerationStatistics{Model: model, Tokens: llm.ReportedTokens(u), DurationMicros: s.micros, Source: "client_clock"}
}

func unknownUsage() llm.Usage {
	return llm.Usage{Unknown: []llm.UsageField{llm.UsageInput, llm.UsageCachedInput, llm.UsageCacheWriteInput, llm.UsageOutput, llm.UsageReasoning}}
}
