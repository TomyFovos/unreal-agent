// Package claudeagent runs the official Claude Code CLI on the user's own
// subscription login as Unreal's Claude route (U21). Claude Code runs its own
// tools. Unreal decides their approvals with its permission mode, records
// every call and result in the Session, and handles shell commands Claude
// Code declines as ordinary Bash requests under that mode (U22). No MCP
// server, mod or API key is involved.
package claudeagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

// ProviderID is the Claude route in /model.
const ProviderID = "claude"

// batchHookSeconds bounds one PostToolBatch wait, which includes a human
// approving a refused command and Unreal running it.
const batchHookSeconds = 24 * 60 * 60

const maxLine = 64 << 20

// Config names the CLI. The Session's runtime fills in the rest per Session.
type Config struct {
	// CompactThresholdTokens defaults to 200,000 for idle compaction.
	CompactThresholdTokens int64             `json:"-"` // injected from Context; not immutable runtime identity
	Binary                 string            // absolute path of the claude CLI
	Directory              string            `json:"-"` // the workspace; Claude Code's working directory
	Results                *observed.Results `json:"-"`
	Environ                func() []string   `json:"-"` // nil means os.Environ
	Subagents              SubagentCLI       `json:"-"`
	// Namespace is the Host's Session store. Two instances can each have a
	// Session of the same name; each needs its own Claude Code conversation.
	Namespace string `json:"-"`
}

// SubagentCLI lets Claude drive the Session's children through Bash with the
// Host's own "subagent" command (U21). Zero means the Session has none.
type SubagentCLI struct {
	Binary    string   // stable kit launcher, or the executable for a source build
	Socket    string   // the Host's gateway socket
	Templates []string // the child templates, for the prompt
}

// The environment that tells the subagent command its Host and Session.
const (
	SubagentSocketEnv  = "UNREAL_AGENT_SOCKET"
	SubagentSessionEnv = "UNREAL_AGENT_SESSION"
)

type Client struct {
	config        Config
	approver      approver
	mu            sync.Mutex
	contextTokens map[string]int64
}

func NewClient(config Config) (*Client, error) {
	if !filepath.IsAbs(config.Binary) {
		return nil, &Error{Code: "cli_missing", Message: "the claude CLI path must be absolute"}
	}
	if !filepath.IsAbs(config.Directory) {
		return nil, &Error{Code: "invalid_workspace"}
	}
	if config.Results == nil {
		return nil, &Error{Code: "results_required"}
	}
	if config.Environ == nil {
		config.Environ = os.Environ
	}
	if !filepath.IsAbs(config.Subagents.Binary) || !filepath.IsAbs(config.Subagents.Socket) {
		config.Subagents = SubagentCLI{}
	}
	if config.CompactThresholdTokens <= 0 {
		config.CompactThresholdTokens = DefaultCompactThresholdTokens
	}
	return &Client{config: config, approver: approver{cli: config.Subagents.Binary}, contextTokens: map[string]int64{}}, nil
}

func (c *Client) Close() error { return nil }

// Error says why a Claude Code turn did not complete. Message is Claude Code's
// own short reason when it gave one.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "Claude Code: " + e.Code
	}
	return "Claude Code: " + e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error {
	if e.Code == "external_reauth_required" {
		return &credential.Error{Code: e.Code}
	}
	return nil
}

// Respond runs one Claude Code turn. Each turn is its own process that resumes
// the Session's Claude Code conversation.
func (c *Client) Respond(ctx context.Context, request llm.Request, opt llm.RequestOptions) (llm.Response, error) {
	if opt.Tools == nil {
		return llm.Response{}, &Error{Code: "tool_rendezvous_required"}
	}
	session := sessionUUID(c.config.Namespace, opt.Identity.SessionID)
	unlock, err := c.lockConversation(ctx, session)
	if err != nil {
		return llm.Response{}, err
	}
	defer unlock()
	if opt.BeginTools != nil {
		if err := opt.BeginTools(ctx, false); err != nil {
			return llm.Response{}, err
		}
	}
	plan := planPrompt(request.Input)
	resume, prompt := plan.resume, plan.prompt
	before := c.lastContextTokens(session, opt)
	// Idle maintenance normally takes care of this. Only the emergency
	// threshold may delay a new input after maintenance was interrupted.
	if before > c.config.CompactThresholdTokens && before-c.config.CompactThresholdTokens > c.config.CompactThresholdTokens {
		c.compact(ctx, request, opt, session, before)
	}
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	for attempt := 0; ; attempt++ {
		response, err := c.run(ctx, request, opt, session, resume, prompt, nil)
		var started *startError
		if attempt > 0 || !errors.As(err, &started) {
			return response, err
		}
		// Claude Code's own record decides: start the conversation it lacks,
		// or continue the one it already has.
		switch started.kind {
		case "missing":
			resume, prompt = false, plan.full
		case "in_use":
			resume = true
		default:
			return response, err
		}
	}
}

// startError is a CLI that exited before the turn began.
type startError struct {
	kind    string // missing, in_use
	message string
}

func (e *startError) Error() string { return "Claude Code: " + e.message }

func (c *Client) run(ctx context.Context, request llm.Request, opt llm.RequestOptions, session string, resume bool, prompt string, compact *compaction) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool", "stdio", "--replay-user-messages"}
	args = append(args, "--disallowedTools", "Agent", "Monitor", "AskUserQuestion")
	if resume {
		args = append(args, "--resume", session)
	} else {
		args = append(args, "--session-id", session)
	}
	if request.Model.ID != "" {
		args = append(args, "--model", request.Model.ID)
	}
	if request.Model.ReasoningEffort.Valid() {
		args = append(args, "--effort", string(request.Model.ReasoningEffort))
	}
	subagents := c.config.Subagents
	if opt.Identity.SessionID == "" {
		subagents = SubagentCLI{}
	}
	args = append(args, "--append-system-prompt", appendedPrompt(modeLabel(permission.ControllerFromContext(ctx)), subagents)+projectInstructions(c.config.Directory))

	// The process outlives ctx briefly: an interrupted turn first asks Claude
	// Code to stop, which also ends the command it runs.
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	cmd := exec.CommandContext(processCtx, c.config.Binary, args...)
	cmd.Dir = c.config.Directory
	cmd.Env = environment(c.config.Environ())
	if subagents.Binary != "" {
		cmd.Env = append(cmd.Env, SubagentSocketEnv+"="+subagents.Socket, SubagentSessionEnv+"="+opt.Identity.SessionID)
	}
	isolate(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure", Message: err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return llm.Response{}, &Error{Code: "subprocess_failure", Message: err.Error()}
	}
	var stderr tail
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return llm.Response{}, &Error{Code: "cli_missing", Message: err.Error()}
	}
	out := &writer{w: stdin}
	t := newTurn(ctx, cancel, opt, out, c.config.Results, &c.approver, c.config.Directory, session)
	stopInterrupt := context.AfterFunc(ctx, func() {
		if compact != nil {
			// Do not let an interrupted summary be saved during graceful
			// shutdown. Reap this process before the next resume can start.
			killGroup(cmd)
			cancel()
			return
		}
		_ = out.send(map[string]any{"type": "control_request", "request_id": "unreal-interrupt", "request": map[string]any{"subtype": "interrupt"}})
		time.AfterFunc(3*time.Second, cancel)
	})
	defer stopInterrupt()
	finishProcess := sync.OnceFunc(func() {
		// Closing stdin ends the CLI after it has saved the conversation; a
		// stuck one is stopped, then killed with everything it started.
		_ = stdin.Close()
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cancel()
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				killGroup(cmd)
				<-exited
			}
		}
		cancel()
		t.workers.Wait()
	})
	defer finishProcess()
	hooks := map[string]any{
		"PreToolUse":    []any{map[string]any{"matcher": gateMatcher, "hookCallbackIds": []string{gateHook}}},
		"PostToolBatch": []any{map[string]any{"hookCallbackIds": []string{batchHook}, "timeout": batchHookSeconds}},
	}
	if err = out.send(map[string]any{"type": "control_request", "request_id": "unreal-init", "request": map[string]any{"subtype": "initialize", "hooks": hooks}}); err == nil {
		err = out.send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}, "parent_tool_use_id": nil, "session_id": ""})
	}
	if err != nil {
		finishProcess()
		return llm.Response{}, startFailure(stderr.String(), &Error{Code: "subprocess_failure", Message: err.Error()})
	}

	reader := bufio.NewReaderSize(stdout, 64<<10)
	var result *frame
	var lastInput *int64
	for result == nil {
		line, readErr := readLine(reader)
		if len(line) > 0 {
			var f frame
			if json.Unmarshal(line, &f) == nil {
				if compact != nil {
					compact.handle(f)
					if f.Type == "result" {
						result = &f
					} else if f.Type == "control_request" {
						t.handle(f)
					}
				} else {
					if f.Type == "stream_event" && f.ParentToolUseID == nil {
						var event streamEvent
						if json.Unmarshal(f.Event, &event) == nil && event.Type == "message_start" && event.Message.Usage != nil {
							value := event.Message.Usage.inputTokens()
							lastInput = &value
						}
					}
					result = t.handle(f)
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if err := t.failed(); err != nil {
		return llm.Response{}, err
	}
	if ctx.Err() != nil {
		return llm.Response{}, ctx.Err()
	}
	if result == nil {
		finishProcess()
		return llm.Response{}, startFailure(stderr.String(), &Error{Code: "subprocess_failure", Message: lastLine(stderr.String())})
	}
	if compact == nil {
		// The result's usage sums every model call of the turn; the last
		// message_start is the current context. Use the sum only without one.
		if lastInput == nil && result.Usage != nil {
			value := result.Usage.inputTokens()
			lastInput = &value
		}
		if lastInput != nil {
			c.recordConversation(ctx, opt, session, llm.ConversationRecord{Provider: ProviderID, InputTokens: *lastInput, InputKnown: true})
		}
	}
	if result.IsError || result.Subtype != "success" {
		message := strings.TrimSpace(result.Result)
		if len(result.Errors) > 0 {
			message = strings.Join(result.Errors, "; ")
		}
		if strings.Contains(message, "No conversation found") {
			return llm.Response{}, &startError{kind: "missing", message: message}
		}
		code := "turn_failed"
		if strings.Contains(message, "/login") {
			code = "external_reauth_required"
		}
		return llm.Response{}, &Error{Code: code, Message: clip(message, 2000)}
	}
	if compact != nil {
		return llm.Response{}, nil
	}
	return t.finish(result), nil
}

func startFailure(stderr string, fallback error) error {
	switch {
	case strings.Contains(stderr, "No conversation found"):
		return &startError{kind: "missing", message: lastLine(stderr)}
	case strings.Contains(stderr, "is already in use"):
		return &startError{kind: "in_use", message: lastLine(stderr)}
	case strings.Contains(stderr, "/login"):
		return &Error{Code: "external_reauth_required", Message: lastLine(stderr)}
	}
	return fallback
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxLine {
			return nil, &Error{Code: "malformed_stream", Message: "a stream-json line is too long"}
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimSpace(line), err
	}
}

// environment passes the Host's environment to Claude Code, minus what would
// bill an API key or a cloud account instead of the subscription (U5), and
// minus markers that would attach it to another Claude Code session.
func environment(env []string) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_EFFORT": true,
	}
	var out []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if drop[key] || strings.HasPrefix(key, "CLAUDE_CODE_USE_") || sessionLink(key) || key == SubagentSocketEnv || key == SubagentSessionEnv {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func sessionLink(key string) bool {
	switch key {
	case "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SSE_PORT":
		return true
	}
	return false
}

// sessionUUID gives every Unreal Session one stable Claude Code conversation,
// distinct per Host Session store.
func sessionUUID(namespace, session string) string {
	if session == "" {
		return uuid.New().String()
	}
	key := "unreal-agent/claude-code/" + session
	if namespace != "" {
		key = "unreal-agent/claude-code/" + namespace + "\x00" + session
	}
	sum := sha256.Sum256([]byte(key))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x80 // version 8: application-defined
	id[8] = id[8]&0x3f | 0x80 // RFC 9562 variant
	return id.String()
}

// tail keeps the end of the CLI's stderr for failure messages.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16<<10 {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-8<<10:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" && !strings.HasPrefix(line, "Warning:") {
			return clip(line, 500)
		}
	}
	return ""
}

var _ io.Writer = (*tail)(nil)
