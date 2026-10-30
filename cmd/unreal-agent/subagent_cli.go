package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// The Host names its gateway and the Session in the environment of the agent
// it runs, so the agent's own shell can drive that Session's children. The
// Claude route has no Unreal tools, only this command through Bash (U21).
const (
	subagentSocketEnv  = "UNREAL_AGENT_SOCKET"
	subagentSessionEnv = "UNREAL_AGENT_SESSION"
)

// defaultWait stays under Claude Code's ten-minute Bash limit.
const defaultWait = 9 * time.Minute

const subagentUsage = `usage: unreal-agent subagent COMMAND
  start [--template lead] [--task TEXT]   start a child; the task is read from stdin without --task
  wait [HANDLE ...] [--timeout 9m]        wait until a child finishes or asks for approval
  approve HANDLE REQUEST allow|deny       answer a child's approval request
  send HANDLE [--text TEXT]               send a child more instructions (stdin without --text)
  cancel HANDLE                           stop a child
  list                                    show this Session's children
  show HANDLE                             show a child's full report`

// isSubagentCommand reports whether args are this command: always for
// unreal-agent, and for unreal only inside an agent's shell, where
// "unreal subagent" would otherwise name a Session.
func isSubagentCommand(name string, args []string) bool {
	return len(args) > 0 && args[0] == "subagent" && (name != "unreal" || os.Getenv(subagentSessionEnv) != "")
}

func runSubagentCLI(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	socket, parent := os.Getenv(subagentSocketEnv), session.ID(os.Getenv(subagentSessionEnv))
	if len(args) == 0 {
		return errors.New(subagentUsage)
	}
	if socket == "" || parent == "" {
		return errors.New("unreal-agent subagent runs inside an Unreal Agent Session's shell (" + subagentSocketEnv + " and " + subagentSessionEnv + " are not set)")
	}
	client := gateway.NewClient(socket)
	defer client.Close()
	c := subagentCLI{client: client, parent: parent, out: out, name: os.Args[0]}
	flags := flag.NewFlagSet("unreal-agent subagent "+args[0], flag.ContinueOnError)
	flags.SetOutput(out)
	switch args[0] {
	case "start":
		template := flags.String("template", "lead", "child template")
		task := flags.String("task", "", "task text; read from stdin when empty")
		if rest, err := parseAll(flags, args[1:]); err != nil || len(rest) != 0 {
			return errors.New(subagentUsage)
		}
		text, err := textArgument(*task, stdin)
		if err != nil {
			return err
		}
		return c.start(ctx, *template, text)
	case "wait":
		timeout := flags.Duration("timeout", defaultWait, "longest wait")
		handles, err := parseAll(flags, args[1:])
		if err != nil {
			return errors.New(subagentUsage)
		}
		return c.wait(ctx, handles, *timeout)
	case "approve":
		if len(args) != 4 || args[3] != "allow" && args[3] != "deny" {
			return errors.New(subagentUsage)
		}
		return c.control(ctx, "approve", args[1], viewer.ControlRequest{Request: args[2], Allow: args[3] == "allow"})
	case "send":
		text := flags.String("text", "", "message; read from stdin when empty")
		handle, err := parseAll(flags, args[1:])
		if err != nil || len(handle) != 1 {
			return errors.New(subagentUsage)
		}
		message, err := textArgument(*text, stdin)
		if err != nil {
			return err
		}
		return c.control(ctx, "send", handle[0], viewer.ControlRequest{Text: message})
	case "cancel":
		if len(args) != 2 {
			return errors.New(subagentUsage)
		}
		return c.control(ctx, "cancel", args[1], viewer.ControlRequest{})
	case "list":
		children, _, err := c.children(ctx)
		if err != nil {
			return err
		}
		if len(children) == 0 {
			fmt.Fprintln(out, "No children in this Session.")
		}
		for _, child := range children {
			child.summary(out)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New(subagentUsage)
		}
		child, _, err := c.child(ctx, args[1])
		if err != nil {
			return err
		}
		child.report(out, c.name, true)
		return nil
	}
	return errors.New(subagentUsage)
}

// parseAll accepts flags before and after the positional arguments.
func parseAll(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		if args = flags.Args(); len(args) == 0 {
			return positional, nil
		}
		positional, args = append(positional, args[0]), args[1:]
	}
}

func textArgument(value string, stdin io.Reader) (string, error) {
	if value == "" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("the text is empty")
	}
	return value, nil
}

type subagentCLI struct {
	client *gateway.Client
	parent session.ID
	out    io.Writer
	name   string // this command as invoked, for the next commands it suggests
}

// childState is one child as its parent's start Operation records it.
type childState struct {
	handle    operation.ID
	child     session.ID
	template  string
	task      string
	status    operation.Status
	runtime   *sessionstore.RuntimeSelection
	finish    *sessionstore.FinishRecord
	approvals []subagent.Approval
	message   string
}

func (s childState) running() bool {
	return s.status != operation.StatusCompleted && s.status != operation.StatusFailed && s.status != operation.StatusCanceled
}

func (c subagentCLI) children(ctx context.Context) ([]childState, host.View, error) {
	view, err := c.client.Inspect(ctx, c.parent, 0, 1)
	if err != nil {
		return nil, view, fmt.Errorf("cannot reach this Session's Host: %w", err)
	}
	var children []childState
	for _, op := range view.Operations {
		plan, err := subagent.DecodePlan(op)
		if err != nil || plan.Action != "start" {
			continue
		}
		state := childState{handle: op.ID, child: plan.ChildID, template: plan.Template, task: plan.Text, status: op.Status}
		if plan.Configuration != nil {
			state.runtime = sessionstore.SelectionFromConfiguration(plan.Configuration.Runtime)
		}
		if handle, err := subagent.DecodeHandle(op); err == nil {
			state.finish, state.approvals = handle.Finish, handle.Approvals
		}
		if remote, err := operation.DecodeRemoteJobState(op); err == nil {
			state.message = remote.TerminalError
			if state.message == "" && state.finish == nil {
				state.message = remote.TerminalResult
			}
		}
		if op.Denial != nil {
			state.message = op.Denial.Error()
		}
		children = append(children, state)
	}
	return children, view, nil
}

func (c subagentCLI) child(ctx context.Context, handle string) (childState, host.View, error) {
	children, view, err := c.children(ctx)
	if err != nil {
		return childState{}, view, err
	}
	for _, child := range children {
		if string(child.handle) == handle {
			return child, view, nil
		}
	}
	return childState{}, view, fmt.Errorf("this Session has no child %q; see %s subagent list", handle, c.name)
}

func (c subagentCLI) start(ctx context.Context, template, task string) error {
	view, err := c.client.Inspect(ctx, c.parent, 0, 1)
	if err != nil {
		return fmt.Errorf("cannot reach this Session's Host: %w", err)
	}
	input := inbox.ID("cli-start-" + uuid.New().String())
	if _, err = modelExchange(ctx, c.client.Extension, modelRequest{Action: "child.start", ID: c.parent, Generation: view.Generation, InputID: input, Template: template, Task: task}); err != nil {
		return fmt.Errorf("could not start %q: %w", template, err)
	}
	handle := subagent.StartOperationID(c.parent, input)
	// The Session dispatches the start after accepting it; return once the
	// child exists, so a wait right after this sees it.
	for deadline := time.Now().Add(15 * time.Second); ; {
		if _, _, err = c.child(ctx, string(handle)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("started %s as %s, but the Session has not shown it yet; check %s subagent list", template, handle, c.name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	fmt.Fprintf(c.out, "Started %s as %s.\nEnd your turn; completion will be sent as a message. Report: %s subagent show %s\n", template, handle, c.name, handle)
	return nil
}

// wait returns when a watched child finishes or asks for approval, or when
// timeout passes. Without handles it watches every running child.
func (c subagentCLI) wait(ctx context.Context, handles []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	watched := map[operation.ID]bool{}
	for _, h := range handles {
		watched[operation.ID(h)] = true
	}
	seen := map[string]bool{}
	first := true
	for {
		children, _, err := c.children(ctx)
		if err != nil {
			return err
		}
		var selected []childState
		for _, child := range children {
			if len(handles) == 0 && first && child.running() {
				watched[child.handle] = true
			}
			if watched[child.handle] {
				selected = append(selected, child)
			}
		}
		if len(handles) > 0 && len(selected) != len(handles) {
			return fmt.Errorf("this Session has no child among %s; see %s subagent list", strings.Join(handles, " "), c.name)
		}
		if first && len(selected) == 0 {
			// Nothing runs anymore: report the latest child in full, which a
			// wait right after a quick child's start is about.
			fmt.Fprintln(c.out, "No running children.")
			for i, child := range children {
				if i == len(children)-1 {
					child.report(c.out, c.name, false)
				} else {
					child.summary(c.out)
				}
			}
			return nil
		}
		first = false
		ready := false
		for _, child := range selected {
			if !child.running() {
				ready = true
			}
			for _, a := range child.approvals {
				if !seen[string(child.handle)+"\x00"+a.ID] {
					seen[string(child.handle)+"\x00"+a.ID] = true
					ready = true
				}
			}
		}
		if ready || !time.Now().Before(deadline) {
			for _, child := range selected {
				child.report(c.out, c.name, false)
			}
			if !ready {
				fmt.Fprintln(c.out, "Still running. End your turn; completion will be sent as a message.")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c subagentCLI) control(ctx context.Context, action, handle string, request viewer.ControlRequest) error {
	child, view, err := c.child(ctx, handle)
	if err != nil {
		return err
	}
	if action == "send" && !child.running() {
		fmt.Fprintln(c.out, subagent.EndedMessage(child.handle, child.status))
		return nil
	}
	request.ParentID, request.ParentGeneration = c.parent, view.Generation
	request.OperationID, request.ChildID = child.handle, child.child
	request.InputID = inbox.ID("cli-" + action + "-" + uuid.New().String())
	remote := viewer.Remote{ParentID: c.parent, Call: c.client.Extension}
	switch action {
	case "send":
		_, err = remote.SteerChild(ctx, request)
	case "cancel":
		_, err = remote.CancelChild(ctx, request)
	case "approve":
		if !slices.ContainsFunc(child.approvals, func(a subagent.Approval) bool { return a.ID == request.Request }) {
			return fmt.Errorf("%s is not waiting for request %q", handle, request.Request)
		}
		_, err = remote.ApproveChild(ctx, request)
	}
	if err != nil {
		return fmt.Errorf("could not %s %s: %w", action, handle, err)
	}
	switch {
	case action == "approve" && request.Allow:
		fmt.Fprintf(c.out, "Allowed request %s of %s.\n", request.Request, handle)
	case action == "approve":
		fmt.Fprintf(c.out, "Denied request %s of %s.\n", request.Request, handle)
	case action == "send":
		fmt.Fprintf(c.out, "Sent to %s.\n", handle)
	default:
		fmt.Fprintf(c.out, "Canceled %s.\n", handle)
	}
	return nil
}

func (s childState) label() string {
	label := s.template
	if s.runtime != nil {
		label += " (" + s.runtime.Model
		if s.runtime.Effort != "" {
			label += " " + string(s.runtime.Effort)
		}
		label += ")"
	}
	return label
}

func (s childState) summary(out io.Writer) {
	line := fmt.Sprintf("%s  %s  %s", s.handle, s.label(), s.status)
	switch {
	case len(s.approvals) > 0:
		line += fmt.Sprintf("  waiting for %d approval(s)", len(s.approvals))
	case s.finish != nil:
		line += "  " + firstLine(s.finish.Result.Summary)
	case s.message != "":
		line += "  " + firstLine(s.message)
	}
	fmt.Fprintln(out, line)
}

func (s childState) report(out io.Writer, name string, full bool) {
	fmt.Fprintf(out, "== %s  %s  %s\n", s.handle, s.label(), s.status)
	if full {
		fmt.Fprintf(out, "task: %s\n", s.task)
	}
	for _, a := range s.approvals {
		fmt.Fprintf(out, "approval needed: request %s: %s %s\n  answer: %s subagent approve %s %s allow|deny\n", a.ID, a.Tool, a.Scope, name, s.handle, a.ID)
	}
	if s.finish != nil {
		r := s.finish.Result
		fmt.Fprintf(out, "result: %s\nsummary: %s\n", r.Status, r.Summary)
		list := func(name string, items []string) {
			if len(items) == 0 {
				fmt.Fprintf(out, "%s: none\n", name)
				return
			}
			fmt.Fprintf(out, "%s:\n", name)
			for _, item := range items {
				fmt.Fprintf(out, "  - %s\n", item)
			}
		}
		list("changed files", r.ChangedFiles)
		list("tests", r.Tests)
		list("blockers", r.Blockers)
	} else if s.message != "" && !s.running() {
		fmt.Fprintf(out, "%s\n", s.message)
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 160 {
		line = line[:157] + "..."
	}
	return line
}

// claudeSubagents tells the Claude route how its Bash reaches this Host's
// children: this executable's subagent command and the gateway socket.
func claudeSubagents(templates map[string]subagent.Template, socket string) (claudeagent.SubagentCLI, error) {
	if len(templates) == 0 {
		return claudeagent.SubagentCLI{}, nil
	}
	binary, err := os.Executable()
	if err != nil {
		return claudeagent.SubagentCLI{}, err
	}
	if binary, err = filepath.Abs(binary); err != nil {
		return claudeagent.SubagentCLI{}, err
	}
	if home, err := os.UserHomeDir(); err == nil {
		binary = subagentCLIPath(binary, home)
	}
	if socket, err = filepath.Abs(socket); err != nil {
		return claudeagent.SubagentCLI{}, err
	}
	names := slices.Sorted(maps.Keys(templates))
	return claudeagent.SubagentCLI{Binary: binary, Socket: socket, Templates: names}, nil
}

func subagentCLIPath(binary, home string) string {
	// Only kit installations use the kit's launcher. A source build must keep
	// its own CLI rather than pick up an unrelated installed executable.
	root := filepath.Join(home, ".local", "opt", "unreal-agent-pilot") + string(filepath.Separator)
	if strings.HasPrefix(binary, root) {
		stable := filepath.Join(home, ".local", "bin", "unreal-agent")
		if info, err := os.Stat(stable); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return stable
		}
	}
	return binary
}
