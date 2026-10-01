package subagent_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	subtool "github.com/unreallabsai/unreal-agent/harness/tool/subagent"
)

// Production bounded child templates intentionally deny ambient shell access.
// This injected runtime grants it only to a local fixture, to verify recovery
// of an already accepted shell operation without expanding production policy.
func TestInterruptedShellChildHelper(t *testing.T) {
	if os.Getenv("UNREAL_SHELL_CHILD") == "" {
		return
	}
	err := sub.Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, c sub.ChildConfig, send sub.Sender) (*host.Session, io.Closer, error) {
		if err := os.MkdirAll(filepath.Join(c.Workspace, "shell-captures"), 0700); err != nil {
			return nil, nil, err
		}
		model := adapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
			for _, item := range r.Input {
				if call, ok := item.Data.(llm.ToolCall); ok && call.Name == "Bash" {
					return llm.Response{}, nil
				}
			}
			// The process waits after the effect, so no exit status can have been
			// persisted when the parent fixture kills both owners.
			command := "printf 'effect\\n' >> '" + strings.ReplaceAll(filepath.Join(c.Workspace, "effect"), "'", "'\\''") + "'; exec sleep 60"
			args, _ := json.Marshal(map[string]string{"command": command})
			return response("Bash", string(args)), nil
		})
		build := func(ctx context.Context, id session.ID) (host.Runtime, error) {
			owner, _ := host.SessionFromContext(ctx)
			manager, err := sub.NewManager(ctx, sub.Config{Owner: owner, Child: &c, SendParent: send})
			if err != nil {
				return host.Runtime{}, err
			}
			ext, err := subtool.Extensions(id, nil, true)
			if err != nil {
				return host.Runtime{}, err
			}
			registry, err := tool.WithExtensions(tool.NewRegistry(tool.StaticTranslators{Bash: bash.New(bash.Config{Shell: "/bin/sh", Directory: c.Workspace, BaseDirectory: filepath.Join(c.Workspace, "shell-captures")})}, "Bash"), ext)
			if err != nil {
				return host.Runtime{}, err
			}
			builder := contextbuilder.NewBuilder()
			for _, d := range registry.StaticDefinitions() {
				builder.AddTool(d.Tool)
			}
			return host.Runtime{Builder: builder, LLM: model, Tools: registry, Operations: operation.NewLocalOperationManager(ctx, manager), Close: manager.Close}, nil
		}
		h, err := host.New(ctx, host.Config{Directory: c.SessionDirectory, Build: build})
		if err != nil {
			return nil, nil, err
		}
		raw, _ := json.Marshal(c)
		child, err := h.Open(ctx, host.Options{ID: c.ChildID, Lifecycle: "child", Configuration: raw, Initial: []inbox.Input{c.InitialInput()}, Policy: permission.Unrestricted()})
		if err != nil {
			h.Close()
			return nil, nil, err
		}
		if err = os.WriteFile(filepath.Join(c.Workspace, "shell-child.pid"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			h.Close()
			return nil, nil, err
		}
		return child, h, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

func shellParent(t *testing.T, dir string, mode host.Mode) (*host.Host, *host.Session, sub.Template) {
	t.Helper()
	binary, _ := os.Executable()
	template := sub.Template{Workspace: dir, Runtime: jsontext.Value("{}"), Policy: permission.Config{Tools: []string{"Finish", "Bash"}}}
	model := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	})
	h, err := host.New(context.Background(), host.Config{Directory: dir, Build: factory(sub.Config{Directory: dir, Binary: binary,
		Arguments: []string{"-test.run=^TestInterruptedShellChildHelper$"}, Environment: append(os.Environ(), "UNREAL_SHELL_CHILD=1"), Templates: map[string]sub.Template{"default": template}}, model)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := h.Open(t.Context(), host.Options{ID: "parent", Mode: mode, Policy: permission.Unrestricted()})
	if err != nil {
		h.Close()
		t.Fatal(err)
	}
	return h, s, template
}

func TestInterruptedShellParentHelper(t *testing.T) {
	dir := os.Getenv("UNREAL_SHELL_PARENT")
	if dir == "" {
		return
	}
	_, parent, template := shellParent(t, dir, host.Create)
	spec, _ := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: parent.ID, Template: "default", Configuration: &template, Text: "shell"})
	if _, err := parent.SubmitOperation(t.Context(), parent.Generation, "spawn", "start", "SubagentStart", spec); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestInterruptedChildShellRecoveryDoesNotReplaySideEffect(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInterruptedShellParentHelper$")
	cmd.Env = append(os.Environ(), "UNREAL_SHELL_PARENT="+dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	childID := sub.ChildID("parent", "start")
	t.Cleanup(func() {
		if t.Failed() {
			view, err := sub.ReadChild(context.Background(), dir, childID, 0, 256)
			t.Logf("child operations=%+v read error=%v", view.View.Operations, err)
		}
	})
	var shell operation.Operation
	var state operation.ShellState
	awaitBoundary(t, func() bool {
		if data, err := os.ReadFile(filepath.Join(dir, "effect")); err != nil || string(data) != "effect\n" {
			return false
		}
		v, err := sub.ReadChild(ctx, dir, childID, 0, 256)
		if err != nil {
			return false
		}
		for _, op := range v.View.Operations {
			if op.Type == operation.TypeShell {
				st, err := operation.DecodeShellState(op)
				if err == nil && op.Status == operation.StatusAwaiting && st.Phase == operation.ShellPhaseProcess && st.ProcessGroupID != 0 && st.PendingExitCode == nil {
					shell, state = op, st
					return true
				}
			}
		}
		return false
	})
	var childPID int
	data, err := os.ReadFile(filepath.Join(dir, "shell-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Sscan(string(data), &childPID); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-state.ProcessGroupID, syscall.SIGKILL)
	defer syscall.Kill(childPID, syscall.SIGKILL)
	// Freeze both owners to prevent an EOF/exit notification from committing a
	// terminal outcome during the deliberately interrupted persistence window.
	if err = cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(childPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	parentLog, childLog := readLog(t, dir, "parent"), readLog(t, dir, childID)
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if err = syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(-state.ProcessGroupID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"start_recorded", "start_not_recorded"} {
		t.Run(stage, func(t *testing.T) {
			restoreLog(t, dir, "parent", parentLog)
			prefix := childLog
			if stage == "start_not_recorded" {
				prefix = logBoundary(t, childLog, false, func(r boundaryRecord) bool {
					if r.Type != "operation" || r.Data.Operation.ID != shell.ID {
						return false
					}
					st, e := operation.DecodeShellState(r.Data.Operation)
					return e == nil && st.ProcessGroupID != 0
				})
			}
			restoreLog(t, dir, childID, prefix)
			h, owner, _ := shellParent(t, dir, host.Resume)
			defer h.Close()
			awaitBoundary(t, func() bool {
				v, err := sub.ReadChild(t.Context(), dir, childID, 0, 256)
				if err != nil {
					return false
				}
				for _, op := range v.View.Operations {
					if op.ID == shell.ID {
						st, e := operation.DecodeShellState(op)
						if e != nil {
							t.Fatal(e)
						}
						if op.Status == operation.StatusAwaiting {
							return false
						}
						want := "interrupted before an exit status"
						if stage == "start_not_recorded" {
							want = "outcome is unknown"
						}
						if op.Status != operation.StatusFailed || st.Result != nil || !strings.Contains(st.TerminalError, want) || v.Finish != nil {
							t.Fatal("invented shell completion", op.Status, st, v.Finish)
						}
						return true
					}
				}
				return false
			})
			// A post-recovery Inbox barrier proves the child can continue while
			// retaining its interrupted tool result, without issuing Bash again.
			req := sub.ControlRequest{ParentID: owner.ID, ParentGeneration: owner.Generation, OperationID: "start", ChildID: childID, InputID: "recovery-barrier", Text: "continue"}
			if _, err := sub.SteerChild(t.Context(), owner, req); err != nil {
				t.Fatal(err)
			}
			awaitBoundary(t, func() bool {
				v, e := owner.Inspect(0, 256)
				if e != nil {
					t.Fatal(e)
				}
				for _, op := range v.Operations {
					p, e := sub.DecodePlan(op)
					if e == nil && p.Action == "send" && op.Status == operation.StatusCompleted {
						return true
					}
				}
				return false
			})
			data, err := os.ReadFile(filepath.Join(dir, "effect"))
			if err != nil || !bytes.Equal(data, []byte("effect\n")) {
				t.Fatal("shell side effect replayed", string(data), err)
			}
			v, _ := owner.Inspect(0, 256)
			starts := 0
			for _, op := range v.Operations {
				p, e := sub.DecodePlan(op)
				if e == nil && p.Action == "start" {
					starts++
					if p.ChildID != childID || op.ID != "start" {
						t.Fatal("replaced child", p)
					}
				}
			}
			if starts != 1 {
				t.Fatal("second child operation", starts)
			}
		})
	}
}
