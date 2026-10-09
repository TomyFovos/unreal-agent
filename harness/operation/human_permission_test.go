package operation_test

import (
	"context"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func modeManager(t *testing.T, timeout time.Duration) (*operation.LocalOperationManager, *permission.Controller, string, string) {
	t.Helper()
	root := t.TempDir()
	base, err := permission.New(permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	life, cancel := context.WithCancel(t.Context())
	c := permission.NewController(life, base, permission.Unrestricted(), timeout)
	ctx := permission.WithController(permission.WithPolicy(life, permission.Unrestricted()), c)
	files, err := mutation.New(mutation.Config{Root: root, Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := native.NewHandler(ctx, native.Executor{Files: files}, "human")
	if err != nil {
		t.Fatal(err)
	}
	m := operation.NewLocalOperationManager(ctx, handler)
	key, release := c.AcquireControl()
	t.Cleanup(func() {
		release()
		cancel()
		c.Close()
		handler.Close()
		<-m.Done()
		base.Close()
	})
	return m, c, key, root
}
func setMode(t *testing.T, c *permission.Controller, key string, mode permission.Mode) {
	t.Helper()
	rev := c.Snapshot().Revision
	proof := ""
	if mode == permission.FullAccess {
		var err error
		proof, err = c.PrepareFull(key, rev)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetMode(key, rev, mode, proof); err != nil {
		t.Fatal(err)
	}
}
func pendingPermission(t *testing.T, c *permission.Controller) permission.ModeSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, changed := c.Watch()
		if len(s.Pending) > 0 {
			return s
		}
		select {
		case <-changed:
		case <-time.After(time.Until(deadline)):
			t.Fatal("pending permission missing")
		}
	}
}
func nativeOperation(t *testing.T, id operation.ID, request native.Request) operation.Operation {
	t.Helper()
	spec, err := native.Spec(request)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: id, ToolName: request.Action, Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady, State: spec.State}
}
func TestHumanManagerReadonlyDeniesBeforeSideEffects(t *testing.T) {
	m, _, _, root := modeManager(t, time.Second)
	write := nativeOperation(t, "no-write", native.Request{Version: 1, Action: "write", Path: filepath.Join(root, "no.txt"), Content: "never", Expected: &mutation.Revision{Exists: false}})
	shell := newShellOperation(t, "no-process", operation.ShellInput{Shell: testShellPath, Command: "touch forbidden", Directory: root}, t.TempDir(), 256)
	for _, op := range []operation.Operation{write, shell} {
		if err := m.Add(op); err != nil {
			t.Fatal(err)
		}
		result := receiveTerminalOperation(t, m.Updates(), op.ID)
		if result.Denial == nil || result.Permission == nil || result.Permission.State != "read_only" || result.Status != operation.StatusFailed {
			t.Fatal(result)
		}
	}
	for _, path := range []string{"no.txt", "forbidden"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatal("denied side effect occurred")
		}
	}
}
func TestHumanManagerOnceDenyTimeoutCancelAndDuplicate(t *testing.T) {
	for _, which := range []string{"allow", "deny", "timeout", "cancel"} {
		t.Run(which, func(t *testing.T) {
			m, c, key, root := modeManager(t, 100*time.Millisecond)
			setMode(t, c, key, permission.Ask)
			op := newShellOperation(t, "one", operation.ShellInput{Shell: testShellPath, Command: "printf x >> result", Directory: root}, t.TempDir(), 256)
			if err := m.Add(op); err != nil {
				t.Fatal(err)
			}
			if err := m.Add(op); err != nil {
				t.Fatal(err)
			}
			s := pendingPermission(t, c)
			if _, err := os.Stat(filepath.Join(root, "result")); !os.IsNotExist(err) {
				t.Fatal("effect before approval")
			}
			switch which {
			case "allow", "deny":
				if err := c.Decide(key, s.Revision, string(op.ID), which == "allow"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if err := m.Cancel(op.ID, "user cancel"); err != nil {
					t.Fatal(err)
				}
			}
			result := receiveTerminalOperation(t, m.Updates(), op.ID)
			if result.Permission == nil {
				t.Fatal("missing canonical approval")
			}
			if which == "allow" {
				content, err := os.ReadFile(filepath.Join(root, "result"))
				if err != nil || string(content) != "x" || result.Permission.State != "allow_once" || result.Status != operation.StatusCompleted {
					t.Fatal(result, err, string(content))
				}
				if err = m.Add(result); err != nil {
					t.Fatal("canonical replay changed admission", err)
				}
				content, _ = os.ReadFile(filepath.Join(root, "result"))
				if string(content) != "x" {
					t.Fatal("duplicate effect")
				}
			} else {
				if result.Status == operation.StatusCompleted || result.Denial == nil {
					t.Fatal("nonapproval succeeded", result)
				}
				if _, err := os.Stat(filepath.Join(root, "result")); !os.IsNotExist(err) {
					t.Fatal("denied effect")
				}
			}
		})
	}
}
func TestHumanManagerFullNativeWriteEditReadAndBash(t *testing.T) {
	m, c, key, root := modeManager(t, time.Second)
	setMode(t, c, key, permission.FullAccess)
	path := filepath.Join(root, "answer.txt")
	write := nativeOperation(t, "write", native.Request{Version: 1, Action: "write", Path: path, Content: "before", Expected: &mutation.Revision{Exists: false}})
	if err := m.Add(write); err != nil {
		t.Fatal(err)
	}
	result := receiveTerminalOperation(t, m.Updates(), write.ID)
	if result.Status != operation.StatusCompleted || result.Permission.State != "full_access" {
		t.Fatal(result)
	}
	read := nativeOperation(t, "read", native.Request{Version: 1, Action: "read", Path: path})
	if err := m.Add(read); err != nil {
		t.Fatal(err)
	}
	readResult := receiveTerminalOperation(t, m.Updates(), read.ID)
	state, err := operation.DecodeRemoteJobState(readResult)
	if err != nil {
		t.Fatal(err)
	}
	var value native.Result
	err = json.Unmarshal(state.Handle, &value)
	if err != nil || value.Revision == nil {
		t.Fatal(value, err)
	}
	edit := nativeOperation(t, "edit", native.Request{Version: 1, Action: "edit", Path: path, OldText: "before", NewText: "after", Expected: value.Revision})
	if err = m.Add(edit); err != nil {
		t.Fatal(err)
	}
	if result = receiveTerminalOperation(t, m.Updates(), edit.ID); result.Status != operation.StatusCompleted {
		t.Fatal(result)
	}
	shell := newShellOperation(t, "test", operation.ShellInput{Shell: testShellPath, Command: "test \"$(cat answer.txt)\" = after", Directory: root}, t.TempDir(), 256)
	if err = m.Add(shell); err != nil {
		t.Fatal(err)
	}
	result = receiveTerminalOperation(t, m.Updates(), shell.ID)
	if result.Status != operation.StatusCompleted || shellState(t, result).Result.ExitCode != 0 {
		t.Fatal(result)
	}
	setMode(t, c, key, permission.ReadOnly)
	denied := nativeOperation(t, "new-write", native.Request{Version: 1, Action: "write", Path: filepath.Join(root, "denied"), Content: "x", Expected: &mutation.Revision{Exists: false}})
	if err = m.Add(denied); err != nil {
		t.Fatal(err)
	}
	if result = receiveTerminalOperation(t, m.Updates(), denied.ID); result.Status != operation.StatusFailed || result.Denial == nil {
		t.Fatal(result)
	}
}
func TestHumanManagerRevokesRunningProcess(t *testing.T) {
	m, c, key, root := modeManager(t, time.Second)
	setMode(t, c, key, permission.FullAccess)
	op := newShellOperation(t, "long", operation.ShellInput{Shell: testShellPath, Command: "printf ready > started; sleep 30; printf bad > late", Directory: root}, t.TempDir(), 256)
	if err := m.Add(op); err != nil {
		t.Fatal(err)
	}
	waitForFileContents(t, filepath.Join(root, "started"), "ready")
	setMode(t, c, key, permission.ReadOnly)
	result := receiveTerminalOperation(t, m.Updates(), op.ID)
	if result.Status != operation.StatusCanceled {
		t.Fatal("revoked process succeeded", result)
	}
	if _, err := os.Stat(filepath.Join(root, "late")); !os.IsNotExist(err) {
		t.Fatal("process survived revoke")
	}
}
