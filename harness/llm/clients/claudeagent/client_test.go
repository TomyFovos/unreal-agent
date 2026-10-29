package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

type harness struct {
	t         *testing.T
	client    *Client
	results   *observed.Results
	workspace string
	args      string

	mu        sync.Mutex
	responses []llm.Response
	commands  []string
	progress  strings.Builder
}

func newHarness(t *testing.T, scenario string, env ...string) *harness {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, results: observed.NewResults(), workspace: t.TempDir()}
	h.args = filepath.Join(t.TempDir(), "args")
	env = append(env, "CLAUDEAGENT_FAKE="+scenario, "CLAUDEAGENT_ARGS="+h.args)
	h.client, err = NewClient(Config{Binary: binary, Directory: h.workspace, Results: h.results, Environ: func() []string { return append(os.Environ(), env...) }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// tools stands in for the coordinator: it records each round and returns the
// receipts Unreal would produce. Claude Code's own calls become value receipts;
// a refused command is "run" by Unreal's Bash.
func (h *harness) tools(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.responses = append(h.responses, response)
	translator := observed.New(h.results)
	var outcomes []llm.ToolOutcome
	for _, item := range response.Output {
		call, ok := item.Data.(llm.ToolCall)
		if !ok {
			continue
		}
		switch call.Name {
		case tool.ClaudeCodeName:
			var submitted operation.Spec
			status := translator.Translate(specs(func(s operation.Spec) { submitted = s }), call)
			op := operation.Operation{ID: "op", Type: submitted.Type, Version: submitted.Version, Status: operation.StatusCompleted, State: submitted.State}
			ops := []operation.Operation{op}
			if status.Error != "" {
				ops = nil
			}
			result, err := translator.TranslateResult(call.CallID, status, ops)
			if err != nil {
				return nil, err
			}
			outcomes = append(outcomes, llm.ToolOutcome{Result: result, Failed: translator.ResultFailed(status, ops)})
		case tool.BashName:
			var arguments struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal([]byte(call.Arguments), &arguments)
			h.commands = append(h.commands, arguments.Command)
			outcomes = append(outcomes, llm.ToolOutcome{Result: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Everything up-to-date"}}}})
		}
	}
	return outcomes, nil
}

type specs func(operation.Spec)

func (s specs) Submit(spec operation.Spec) operation.ID { s(spec); return "op" }

func (h *harness) respond(ctx context.Context, input ...llm.Item) (llm.Response, error) {
	if len(input) == 0 {
		input = []llm.Item{user("Change hello to goodbye.")}
	}
	return h.client.Respond(ctx, llm.Request{Model: llm.Model{ID: "opus", ReasoningEffort: llm.ReasoningEffortMax}, Input: input}, llm.RequestOptions{
		Identity: llm.RequestIdentity{SessionID: "session-1"},
		Tools:    h.tools,
		Progress: func(p llm.Progress) { h.mu.Lock(); h.progress.WriteString(p.Delta); h.mu.Unlock() },
	})
}

func user(text string) llm.Item {
	return llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}
}

func assistant(text string, own bool) llm.Item {
	item := llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}
	if own {
		item.ProviderID = itemPrefix + "msg_old"
	}
	return item
}

func withMode(t *testing.T, mode permission.Mode) (context.Context, *permission.Controller, string) {
	t.Helper()
	maximum, err := permission.New(permission.Config{Tools: tool.StaticNames(), FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	controller := permission.NewController(context.Background(), nil, maximum, time.Minute)
	t.Cleanup(controller.Close)
	key, release := controller.AcquireControl()
	t.Cleanup(release)
	revision := controller.Snapshot().Revision
	switch mode {
	case permission.Ask:
		if err = controller.SetMode(key, revision, permission.Ask); err != nil {
			t.Fatal(err)
		}
	case permission.FullAccess:
		err = controller.SetMode(key, revision, permission.FullAccess)
		if err != nil {
			t.Fatal(err)
		}
	}
	return permission.WithController(context.Background(), controller), controller, key
}

func (h *harness) editScenario(t *testing.T) string {
	path := filepath.Join(h.workspace, "sample.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFullAccessEditRunsAndIsRecorded(t *testing.T) {
	h := newHarness(t, "edit")
	path := h.editScenario(t)
	h.client.config.Environ = appendEnv(h.client.config.Environ, "CLAUDEAGENT_FILE="+path, "CLAUDEAGENT_CWD="+h.workspace)
	ctx, _, _ := withMode(t, permission.FullAccess)
	response, err := h.respond(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "goodbye world\n" {
		t.Fatalf("file = %q, want the allowed edit applied", got)
	}
	if len(h.responses) != 1 {
		t.Fatalf("rounds = %d, want 1", len(h.responses))
	}
	round := h.responses[0].Output
	if len(round) != 2 || round[0].Data.(llm.Message).Text != "Editing the file." {
		t.Fatalf("round = %+v, want the text before the call and the call", round)
	}
	call := round[1].Data.(llm.ToolCall)
	if call.Name != tool.ClaudeCodeName || call.CallID != "toolu_1" || !strings.Contains(call.Arguments, `"tool":"Edit"`) || !strings.HasPrefix(round[1].ProviderID, itemPrefix) {
		t.Fatalf("recorded call = %+v (%q)", call, round[1].ProviderID)
	}
	if len(response.Output) != 1 || response.Output[0].Data.(llm.Message).Text != "Done." || !strings.HasPrefix(response.Output[0].ProviderID, itemPrefix) {
		t.Fatalf("final response = %+v", response.Output)
	}
	// Each recorded response carries only its own model steps, and its output
	// rate counts streaming time only, never the tool or approval wait.
	first := h.responses[0]
	if u := first.Usage; u.InputTokens != 1110 || u.CachedInputTokens != 1000 || u.CacheWriteInputTokens != 10 || u.OutputTokens != 20 {
		t.Fatalf("round usage = %+v", u)
	}
	if u := response.Usage; u.InputTokens != 1105 || u.CachedInputTokens != 1100 || u.OutputTokens != 7 {
		t.Fatalf("final usage = %+v", u)
	}
	for _, g := range []*llm.GenerationStatistics{first.Generation, response.Generation} {
		// Pipe buffering and scheduling can compress or extend the interval
		// this process observes. Exact timing and excluded waits are checked
		// with a controlled clock in TestGenerationTimingExcludesToolAndApprovalWaits.
		if g == nil || !g.Valid() {
			t.Fatalf("generation statistics = %+v", g)
		}
	}
	if !strings.Contains(h.progress.String(), "Editing") || !strings.Contains(h.progress.String(), "Claude Code Edit: "+path) {
		t.Fatalf("progress = %q", h.progress.String())
	}
	args, _ := os.ReadFile(h.args)
	for _, want := range []string{"--session-id", sessionUUID("", "session-1"), "--model\nopus", "--effort\nmax", "--permission-prompt-tool\nstdio", "--disallowedTools\nAgent\nMonitor\nAskUserQuestion", "Full Access"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("argv lacks %q:\n%s", want, args)
		}
	}
}

func TestMultipleStepsReportLastContextAndSummedUsage(t *testing.T) {
	h := newHarness(t, "multiple_steps")
	response, err := h.respond(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.responses) != 1 {
		t.Fatalf("tool boundaries = %d, want 1", len(h.responses))
	}
	for i, tc := range []struct {
		response                                llm.Response
		input, cached, written, output, context int64
	}{
		{h.responses[0], 3330, 3000, 30, 50, 2220},
		{response, 7140, 7000, 70, 90, 4080},
	} {
		u, g := tc.response.Usage, tc.response.Generation
		if u.InputTokens != tc.input || u.CachedInputTokens != tc.cached || u.CacheWriteInputTokens != tc.written || u.OutputTokens != tc.output {
			t.Fatalf("boundary %d: usage lost its sum: %+v", i, u)
		}
		if g == nil || !g.Valid() || g.Tokens.Input != tc.input || g.Tokens.Output != tc.output || !g.ContextKnown || g.ContextTokens != tc.context {
			t.Fatalf("boundary %d: last context and summed tokens = %+v", i, g)
		}
	}
}

func TestReadOnlyDeniesTheEditAndRecordsTheDenial(t *testing.T) {
	h := newHarness(t, "edit")
	path := h.editScenario(t)
	h.client.config.Environ = appendEnv(h.client.config.Environ, "CLAUDEAGENT_FILE="+path)
	ctx, _, _ := withMode(t, permission.ReadOnly)
	if _, err := h.respond(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "hello world\n" {
		t.Fatalf("file = %q, want it unchanged", got)
	}
	call := h.responses[0].Output[1].Data.(llm.ToolCall)
	if call.Name != tool.ClaudeCodeName {
		t.Fatalf("call = %+v", call)
	}
}

func TestAskWaitsForTheHumanDecision(t *testing.T) {
	for _, allow := range []bool{true, false} {
		h := newHarness(t, "edit")
		path := h.editScenario(t)
		h.client.config.Environ = appendEnv(h.client.config.Environ, "CLAUDEAGENT_FILE="+path)
		ctx, controller, key := withMode(t, permission.Ask)
		done := make(chan error, 1)
		go func() { _, err := h.respond(ctx); done <- err }()
		var pending permission.Pending
		deadline := time.Now().Add(10 * time.Second)
		for {
			snapshot := controller.Snapshot()
			if len(snapshot.Pending) == 1 {
				pending = snapshot.Pending[0]
				if err := controller.Decide(key, snapshot.Revision, pending.ID, allow); err != nil {
					t.Fatal(err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no approval request reached Unreal")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if pending.ID != "claude:toolu_1" || pending.Tool != tool.EditName || pending.Scope != "Claude Code Edit: "+path {
			t.Fatalf("pending = %+v", pending)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		want := "hello world\n"
		if allow {
			want = "goodbye world\n"
		}
		if got := read(t, path); got != want {
			t.Fatalf("allow=%v: file = %q, want %q", allow, got, want)
		}
	}
}

func TestRefusedCommandRunsInUnrealsShell(t *testing.T) {
	// A deny rule on the whole command and one on a part of it both count.
	for _, reason := range []string{"rule", "subcommandResults"} {
		t.Run(reason, func(t *testing.T) { testRefusedCommand(t, reason) })
	}
}

// A shell call Unreal itself denied is not handed back to Unreal's shell.
func TestUnrealsOwnDenialIsNotRunByUnreal(t *testing.T) {
	h := newHarness(t, "refused", "CLAUDEAGENT_REASON=hook")
	ctx, _, _ := withMode(t, permission.ReadOnly)
	if _, err := h.respond(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.commands) != 0 {
		t.Fatalf("Unreal ran %q although its own Read Only gate denied it", h.commands)
	}
	if call := h.responses[0].Output[0].Data.(llm.ToolCall); call.Name != tool.ClaudeCodeName {
		t.Fatalf("recorded call = %+v, want the denied Claude Code call", call)
	}
}

func testRefusedCommand(t *testing.T, reason string) {
	h := newHarness(t, "refused", "CLAUDEAGENT_REASON="+reason)
	sub := filepath.Join(h.workspace, "sub")
	h.client.config.Environ = appendEnv(h.client.config.Environ, "CLAUDEAGENT_CWD="+sub)
	ctx, _, _ := withMode(t, permission.FullAccess)
	response, err := h.respond(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.commands) != 1 || h.commands[0] != "cd '"+sub+"' && git push" {
		t.Fatalf("Unreal ran %q, want git push where Claude Code's shell was", h.commands)
	}
	call := h.responses[0].Output[0].Data.(llm.ToolCall)
	if call.Name != tool.BashName || call.CallID != "toolu_9" {
		t.Fatalf("recorded call = %+v, want Unreal's Bash under the refused call's id", call)
	}
	text := response.Output[0].Data.(llm.Message).Text
	if !strings.Contains(text, ranPrefix+" `git push`") || !strings.Contains(text, "Everything up-to-date") {
		t.Fatalf("Claude read %q, want Unreal's result", text)
	}
}

func TestReadOnlyGateRefusesCommandsBeforeClaudeCodeRules(t *testing.T) {
	for mode, want := range map[permission.Mode]string{permission.ReadOnly: "gate:deny", permission.Ask: "gate:pass", permission.FullAccess: "gate:pass"} {
		h := newHarness(t, "gate")
		ctx, _, _ := withMode(t, mode)
		response, err := h.respond(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := response.Output[0].Data.(llm.Message).Text; got != want {
			t.Fatalf("%s: %q, want %q", mode, got, want)
		}
	}
}

// A command that only reads passes the Read Only gate, as in Codex.
func TestReadOnlyGatePassesReadOnlyCommands(t *testing.T) {
	h := newHarness(t, "gate", "CLAUDEAGENT_COMMAND=git log --oneline -5 && git status")
	ctx, _, _ := withMode(t, permission.ReadOnly)
	response, err := h.respond(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Output[0].Data.(llm.Message).Text; got != "gate:pass" {
		t.Fatalf("got %q, want the read-only command to pass", got)
	}
	if allowed, reason := h.client.approver.decide(ctx, "Bash", jsontext.Value(`{"command":"git show HEAD"}`), "toolu_r"); !allowed {
		t.Fatalf("Read Only denied a read-only command: %s", reason)
	}
}

// Interrupting a turn asks Claude Code to stop, which ends the command it
// runs, before the process is stopped.
func TestInterruptAsksClaudeCodeToStop(t *testing.T) {
	h := newHarness(t, "interrupt")
	ctx, _, _ := withMode(t, permission.FullAccess)
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := h.respond(ctx, user("run a long command")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the interruption", err)
	}
	if _, err := os.Stat(h.args + ".interrupt"); err != nil {
		t.Fatal("Claude Code was not asked to stop")
	}
}

// After an interrupted run, a resumed Claude Code may first end work it queued
// itself; the turn's answer is the one to Unreal's message.
func TestResultBeforeTheMessageIsTakenUpIsSkipped(t *testing.T) {
	h := newHarness(t, "stale")
	ctx, _, _ := withMode(t, permission.FullAccess)
	response, err := h.respond(ctx, user("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) == 0 || !strings.HasSuffix(response.Output[0].Data.(llm.Message).Text, "hello") {
		t.Fatalf("response = %+v, want the answer to the message", response.Output)
	}
}

func TestMissingConversationStartsAFreshOneWithContext(t *testing.T) {
	h := newHarness(t, "missing")
	ctx, _, _ := withMode(t, permission.FullAccess)
	response, err := h.respond(ctx, user("first"), assistant("earlier answer", true), user("second"))
	if err != nil {
		t.Fatal(err)
	}
	text := response.Output[0].Data.(llm.Message).Text
	if !strings.HasPrefix(text, "fresh:") || !strings.Contains(text, "[user] first") || !strings.Contains(text, "[assistant] earlier answer") || !strings.HasSuffix(text, "New message:\nsecond") {
		t.Fatalf("fresh conversation got %q", text)
	}
}

func TestSessionInUseIsResumed(t *testing.T) {
	h := newHarness(t, "in_use")
	ctx, _, _ := withMode(t, permission.FullAccess)
	response, err := h.respond(ctx, user("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Output[0].Data.(llm.Message).Text; got != "resumed:hello" {
		t.Fatalf("got %q", got)
	}
}

func TestLoginProblemAsksForReauthentication(t *testing.T) {
	h := newHarness(t, "login")
	ctx, _, _ := withMode(t, permission.FullAccess)
	_, err := h.respond(ctx)
	var credentialErr *credential.Error
	if !errors.As(err, &credentialErr) || credentialErr.Code != "external_reauth_required" {
		t.Fatalf("err = %v, want external_reauth_required", err)
	}
}

func TestDiscoverReadsModelsAndAccount(t *testing.T) {
	binary, _ := os.Executable()
	catalog, account, err := Discover(context.Background(), binary, func() []string { return append(os.Environ(), "CLAUDEAGENT_FAKE=catalog") })
	if err != nil {
		t.Fatal(err)
	}
	if !account.Subscription() || account.SubscriptionType != "Claude Team" {
		t.Fatalf("account = %+v", account)
	}
	if len(catalog.Models) != 2 || catalog.Models[0].ID != "opus" || catalog.Models[0].ResolvedModel != "claude-opus-5-5" || catalog.Models[0].DefaultEffort != llm.ReasoningEffortHigh || len(catalog.Models[0].Efforts) != 5 {
		t.Fatalf("models = %+v", catalog.Models)
	}
	if haiku := catalog.Models[1]; haiku.SupportsEffort == nil || *haiku.SupportsEffort || len(haiku.Efforts) != 0 {
		t.Fatalf("a model without effort levels = %+v", haiku)
	}
}

func TestPromptPlan(t *testing.T) {
	first := planPrompt([]llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "system"}}, user("hello")})
	if first.resume || first.prompt != "hello" {
		t.Fatalf("first turn = %+v", first)
	}
	next := planPrompt([]llm.Item{user("hello"), assistant("hi", true), user("again")})
	if !next.resume || next.prompt != "again" {
		t.Fatalf("next turn = %+v", next)
	}
	// Another model answered in between: Claude gets those turns as context.
	switched := planPrompt([]llm.Item{user("hello"), assistant("hi", true), user("ask codex"), assistant("codex answer", false),
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: receiptPrefix + " (historical data):\nok"}}, user("back to claude")})
	if !switched.resume || !strings.Contains(switched.prompt, "[user] ask codex") || !strings.Contains(switched.prompt, "[assistant] codex answer") || !strings.HasSuffix(switched.prompt, "New message:\nback to claude") || strings.Contains(switched.prompt, "[assistant] hi") {
		t.Fatalf("switched turn = %q", switched.prompt)
	}
	// Receipts for Claude Code's own calls are already in its conversation.
	own := planPrompt([]llm.Item{user("go"), {ProviderID: itemPrefix + "toolu_1", Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "toolu_1", Name: tool.ClaudeCodeName}},
		{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "toolu_1", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "done"}}}}})
	if !own.resume || own.prompt != "Unreal Agent resumed this turn after an interruption. Continue the task." {
		t.Fatalf("interrupted turn = %q", own.prompt)
	}
}

func TestEnvironmentKeepsTheSubscription(t *testing.T) {
	got := environment([]string{"PATH=/usr/local/go/bin:/usr/bin", "HOME=/home/u", "ANTHROPIC_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=y", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CONFIG_DIR=/c", "CLAUDE_CODE_OAUTH_TOKEN=t", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=9"})
	want := []string{"PATH=/usr/local/go/bin:/usr/bin", "HOME=/home/u", "CLAUDE_CONFIG_DIR=/c", "CLAUDE_CODE_OAUTH_TOKEN=t", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=9"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

func TestSessionUUIDIsStableAndValid(t *testing.T) {
	a, b := sessionUUID("", "session-1"), sessionUUID("", "session-1")
	if a != b || a == sessionUUID("", "session-2") {
		t.Fatalf("ids %s %s", a, b)
	}
	// Two Host instances may each have a Session named "development".
	if x, y := sessionUUID("/state/default/sessions", "development"), sessionUUID("/state/p-1/sessions", "development"); x == y || x != sessionUUID("/state/default/sessions", "development") {
		t.Fatal("instances share a Claude Code conversation", x, y)
	}
	if _, err := uuid.Parse(a); err != nil {
		t.Fatal(err)
	}
}

func appendEnv(environ func() []string, extra ...string) func() []string {
	return func() []string { return append(environ(), extra...) }
}

// Claude drives the Session's children with the Host's subagent command:
// Claude Code gets the Host and Session in its environment, and Unreal lets
// the exact command through without asking anyone, in every mode (U17).
func TestSubagentCommandReachesTheHostWithoutApproval(t *testing.T) {
	const cli = "/opt/unreal/bin/unreal"
	for _, mode := range []permission.Mode{permission.ReadOnly, permission.Ask, permission.FullAccess} {
		h := newHarness(t, "subagent", "CLAUDEAGENT_CLI="+cli)
		h.client.config.Subagents = SubagentCLI{Binary: cli, Socket: "/run/unreal/host.sock", Templates: []string{"lead"}}
		h.client.approver.cli = cli
		ctx, controller, _ := withMode(t, mode)
		response, err := h.respond(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := response.Output[len(response.Output)-1].Data.(llm.Message).Text; got != "env:/run/unreal/host.sock:session-1 gate:pass use:allow" {
			t.Fatalf("%s: %q", mode, got)
		}
		if pending := controller.Snapshot().Pending; len(pending) != 0 {
			t.Fatalf("%s: the human was asked: %v", mode, pending)
		}
		if args := read(t, h.args); !strings.Contains(args, cli+" subagent start --template lead") {
			t.Fatalf("%s: the prompt does not name the command", mode)
		}
	}
}

// Reading a file the user sent into ~/drop is allowed in every mode.
func TestDroppedFilesAreReadableInReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx, _, _ := withMode(t, permission.ReadOnly)
	var a approver
	if ok, reason := a.decide(ctx, "Read", jsontext.Value(`{"file_path":"`+filepath.Join(home, "drop", "spec.pdf")+`"}`), "t1"); !ok {
		t.Fatalf("Read Only refused a dropped file: %s", reason)
	}
	if ok, _ := a.decide(ctx, "Read", jsontext.Value(`{"file_path":"`+filepath.Join(home, ".codex", "auth.json")+`"}`), "t2"); ok {
		t.Fatal("Read Only allowed a file outside the drop directory")
	}
}

func TestProjectInstructionsKeepLargeInput(t *testing.T) {
	root := t.TempDir()
	instructions := strings.Repeat("project instruction\n", 4000)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(instructions), 0600); err != nil {
		t.Fatal(err)
	}
	if got := projectInstructions(root); !strings.HasSuffix(got, instructions) {
		t.Fatal("large project instructions lost")
	}
}

func TestStartupReasonWaitsForStderrAfterStdoutCloses(t *testing.T) {
	for _, scenario := range []string{"in_use_delayed", "login_delayed"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, scenario)
			ctx, _, _ := withMode(t, permission.FullAccess)
			response, err := h.respond(ctx, user("hello"))
			if scenario == "login_delayed" {
				var authentication *credential.Error
				if !errors.As(err, &authentication) || authentication.Code != "external_reauth_required" {
					t.Fatal("late login reason was lost", err)
				}
			} else if err != nil || len(response.Output) != 1 || response.Output[0].Data.(llm.Message).Text != "resumed:hello" {
				t.Fatal("late in-use reason was lost", response, err)
			}
		})
	}
}

func TestCatalogReasonWaitsForStderrAfterStdoutCloses(t *testing.T) {
	h := newHarness(t, "login_delayed")
	_, _, err := Discover(t.Context(), h.client.config.Binary, h.client.config.Environ)
	var authentication *credential.Error
	if !errors.As(err, &authentication) || authentication.Code != "external_reauth_required" {
		t.Fatal("catalog lost late login reason", err)
	}
}
