package dap

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDAPAdapterProcess(t *testing.T) {
	mode := os.Getenv("UNREAL_DAP_TEST_MODE")
	if mode == "" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	seq := 0
	var launch message
	send := func(typ string, requestSeq int, command string, body any) {
		seq++
		raw, _ := json.Marshal(body)
		msg := message{Seq: seq, Type: typ, RequestSeq: requestSeq, Command: command, Success: true, Body: raw}
		if typ == "event" {
			msg.Event = command
			msg.Command = ""
		}
		data, _ := json.Marshal(msg)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			os.Exit(0)
		}
		count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length: ")))
		if err != nil {
			os.Exit(2)
		}
		if _, err = reader.ReadString('\n'); err != nil {
			os.Exit(2)
		}
		raw := make([]byte, count)
		if _, err = io.ReadFull(reader, raw); err != nil {
			os.Exit(2)
		}
		var msg message
		if json.Unmarshal(raw, &msg) != nil {
			os.Exit(2)
		}
		switch msg.Command {
		case "initialize":
			if mode == "badframe" {
				fmt.Fprint(os.Stdout, "Content-Length: 999999999\r\n\r\n")
				continue
			}
			send("response", msg.Seq, msg.Command, map[string]any{"supportsConfigurationDoneRequest": true})
		case "launch", "attach":
			launch = msg
			send("event", 0, "initialized", map[string]any{})
		case "configurationDone":
			send("response", msg.Seq, msg.Command, map[string]any{})
			send("event", 0, "stopped", map[string]any{"threadId": 1})
			send("response", launch.Seq, launch.Command, map[string]any{})
		case "threads":
			if mode == "terminated" {
				send("event", 0, "terminated", map[string]any{})
			}
			send("response", msg.Seq, msg.Command, map[string]any{"threads": []any{map[string]any{"id": 1, "name": "main"}}})
		case "stackTrace":
			send("response", msg.Seq, msg.Command, map[string]any{"stackFrames": []any{map[string]any{"id": 10, "name": "main", "line": 1, "column": 1, "source": map[string]any{"path": "/fixture/main.go"}}}})
		case "scopes":
			send("response", msg.Seq, msg.Command, map[string]any{"scopes": []any{map[string]any{"name": "locals", "variablesReference": 11}}})
		case "variables":
			send("response", msg.Seq, msg.Command, map[string]any{"variables": []any{map[string]any{"name": "x", "value": "42", "type": "int"}}})
		case "evaluate":
			if mode == "hang" {
				continue
			}
			if mode == "exit" {
				os.Exit(0)
			}
			send("response", msg.Seq, msg.Command, map[string]any{"result": "42", "variablesReference": 0})
		case "setBreakpoints":
			send("response", msg.Seq, msg.Command, map[string]any{"breakpoints": []any{map[string]any{"id": 1, "line": 2, "verified": true}}})
		case "continue":
			send("event", 0, "continued", map[string]any{})
			send("response", msg.Seq, msg.Command, map[string]any{})
		case "pause", "next", "stepIn", "stepOut":
			send("event", 0, "stopped", map[string]any{"threadId": 1})
			send("response", msg.Seq, msg.Command, map[string]any{})
		case "disconnect":
			if path := os.Getenv("UNREAL_DAP_TEST_CLEANUP"); path != "" {
				_ = os.WriteFile(path, msg.Arguments, 0600)
			}
			send("response", msg.Seq, msg.Command, map[string]any{})
			os.Exit(0)
		default:
			os.Exit(2)
		}
	}
}
func testManager(t *testing.T, mode string, policy *permission.Policy) (*Manager, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cleanup := filepath.Join(dir, "cleanup.json")
	m, err := NewManager(permission.WithPolicy(t.Context(), policy), AdapterConfig{ID: "test", Path: exe, Arguments: []string{"-test.run=^TestDAPAdapterProcess$"}, Directory: dir, Environment: []string{"UNREAL_DAP_TEST_MODE=" + mode, "UNREAL_DAP_TEST_CLEANUP=" + cleanup}, AllowedAttachPIDs: []int{1234}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, cleanup
}
func startRequest(m *Manager, command string) Request {
	r := Request{Version: 1, OwnerGeneration: m.Generation(), Handle: Handle{ID: "debug"}, Command: command, Start: &Start{Adapter: "test"}}
	if command == "launch" {
		r.Start.Program = "/fixture/program"
		r.Start.Directory = "/fixture"
	} else {
		r.Start.ProcessID = 1234
	}
	return r
}
func call(t *testing.T, m *Manager, r Request) Result {
	t.Helper()
	out, err := m.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("%s failed: %v (%+v)", r.Command, err, out)
	}
	return out
}
func TestRealProcessTypedDAPCapabilitiesAndStaleReferences(t *testing.T) {
	m, _ := testManager(t, "normal", permission.Unrestricted())
	out := call(t, m, startRequest(m, "launch"))
	if out.Handle.Generation != m.Generation() || out.Status != "stopped" {
		t.Fatal(out)
	}
	r := Request{Version: 1, OwnerGeneration: m.Generation(), Handle: out.Handle, StopEpoch: out.StopEpoch, ThreadID: 1}
	for _, command := range []string{"threads", "stackTrace", "scopes", "variables", "evaluate", "setBreakpoints", "next", "stepIn", "stepOut"} {
		r.Command = command
		r.FrameID = 10
		r.VariablesReference = 11
		r.Expression = "x"
		r.Source = "/fixture/main.go"
		r.Breakpoints = []Breakpoint{{Line: 2}}
		result := call(t, m, r)
		if command == "evaluate" && result.Value != "42" {
			t.Fatal("missing evaluation")
		}
		r.StopEpoch = result.StopEpoch
	}
	oldEpoch := r.StopEpoch
	r.Command = "continue"
	out = call(t, m, r)
	if out.Status != "running" {
		t.Fatal(out)
	}
	r.Command = "pause"
	out = call(t, m, r)
	r.Command = "variables"
	r.StopEpoch = oldEpoch
	if _, err := m.Execute(t.Context(), r); codeOf(err) != "stale_reference" {
		t.Fatal("stale reference accepted", err)
	}
	r.StopEpoch = out.StopEpoch
	r.Handle.Generation = "old"
	if _, err := m.Execute(t.Context(), r); codeOf(err) != "expired" {
		t.Fatal("stale generation accepted", err)
	}
}
func TestAttachDetachOwnershipAndPermission(t *testing.T) {
	m, cleanup := testManager(t, "normal", permission.Unrestricted())
	out := call(t, m, startRequest(m, "attach"))
	r := Request{Version: 1, OwnerGeneration: m.Generation(), Handle: out.Handle, Command: "disconnect", Terminate: true}
	if _, err := m.Execute(t.Context(), r); codeOf(err) != "cannot_terminate_attached_target" {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"terminateDebuggee":true`) {
		t.Fatal("terminated attached target")
	}
	denied, _ := testManager(t, "normal", permission.DenyAll())
	if _, err := denied.Execute(t.Context(), startRequest(denied, "launch")); permission.Failure(err) == nil {
		t.Fatal("permission bypass", err)
	}
	restricted, _ := permission.New(permission.Config{Tools: []string{"DAP"}, ProcessMode: permission.ProcessUnrestricted})
	defer restricted.Close()
	limited, _ := testManager(t, "normal", restricted)
	if _, err := limited.Execute(t.Context(), startRequest(limited, "launch")); permission.Failure(err) == nil {
		t.Fatal("sandbox restriction bypass", err)
	}
}
func TestLaunchTerminateOwnership(t *testing.T) {
	m, cleanup := testManager(t, "normal", permission.Unrestricted())
	call(t, m, startRequest(m, "launch"))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cleanup)
	if err != nil || !strings.Contains(string(raw), `"terminateDebuggee":true`) {
		t.Fatal("launch cleanup did not terminate owned debuggee", err, string(raw))
	}
}
func TestCancellationExitAndInvalidFrames(t *testing.T) {
	for _, mode := range []string{"hang", "exit", "badframe", "terminated"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := testManager(t, mode, permission.Unrestricted())
			out, err := m.Execute(t.Context(), startRequest(m, "launch"))
			if mode == "badframe" {
				if err == nil {
					t.Fatal("invalid frame accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := Request{Version: 1, OwnerGeneration: m.Generation(), Handle: out.Handle, Command: "evaluate", Expression: "x", StopEpoch: out.StopEpoch}
			if mode == "terminated" {
				r.Command = "threads"
				call(t, m, r)
				r.Command = "inspect"
				if _, err := m.Execute(t.Context(), r); codeOf(err) != "expired" {
					t.Fatal(err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if _, err := m.Execute(ctx, r); err == nil {
				t.Fatal("interrupted command succeeded")
			}
			m.mu.Lock()
			session := m.sessions["debug"]
			m.mu.Unlock()
			select {
			case <-session.done:
			case <-time.After(7 * time.Second):
				t.Fatal("adapter process leaked")
			}
		})
	}
}
func TestRestoredOperationsExpireWithoutExecution(t *testing.T) {
	m, _ := testManager(t, "normal", permission.Unrestricted())
	r := startRequest(m, "launch")
	r.OwnerGeneration = "previous-owner"
	spec, err := NewSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "restored", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	if err := m.AddRemoteJob(op); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-m.RemoteJobUpdates():
		if out.Status != operation.StatusFailed {
			t.Fatal("restored launch ran")
		}
		state, err := operation.DecodeRemoteJobState(out)
		if err != nil {
			t.Fatal(err)
		}
		var result Result
		if json.Unmarshal(state.Handle, &result) != nil || result.Error != "expired" {
			t.Fatal("not typed expired")
		}
	case <-time.After(time.Second):
		t.Fatal("no completion")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 0 {
		t.Fatal("recovery launched process")
	}
}
func TestFramingAndUTF8Bounds(t *testing.T) {
	for _, bad := range []string{"Content-Length: -1\r\n\r\n", "Content-Length: 100000000\r\n\r\n", "X: 1\r\n\r\n", strings.Repeat("x", 1025)} {
		if _, _, _, err := frame([]byte(bad)); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	var truncated bool
	value := bounded(strings.Repeat("界", 1000), &truncated)
	if !truncated {
		t.Fatal("no bound")
	}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal("invalid UTF8")
	}
	if _, _, complete, err := frame([]byte("Content-Length: 4\r\n\r\n{}")); err != nil || complete {
		t.Fatal("partial frame not retained")
	}
}

func TestCancellationBeforeDispatchAndBoundedTypedResult(t *testing.T) {
	m, _ := testManager(t, "normal", permission.Unrestricted())
	request := startRequest(m, "launch")
	spec, err := NewSpec(request)
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "cancel-first", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	if err := m.CancelRemoteJob(current.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if err := m.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-m.RemoteJobUpdates():
		if out.Status != operation.StatusCanceled {
			t.Fatal("canceled operation executed")
		}
		state, _ := operation.DecodeRemoteJobState(out)
		var result Result
		if json.Unmarshal(state.Handle, &result) != nil || result.Error != "interrupted" {
			t.Fatal("missing typed interrupted")
		}
	case <-time.After(time.Second):
		t.Fatal("no completion")
	}
	m.mu.Lock()
	count := len(m.sessions)
	m.mu.Unlock()
	if count != 0 {
		t.Fatal("canceled launch started process")
	}
}
func TestAttachAllowlistAndEvaluateCapability(t *testing.T) {
	policy, err := permission.New(permission.Config{Tools: []string{"DAP"}, ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	m, _ := testManager(t, "normal", policy)
	attach := startRequest(m, "attach")
	attach.Start.ProcessID = 999
	if _, err := m.Execute(t.Context(), attach); permission.Failure(err) == nil {
		t.Fatal("attach allowlist bypass")
	}
	started := call(t, m, startRequest(m, "launch"))
	evaluate := Request{Version: 1, OwnerGeneration: m.Generation(), Handle: started.Handle, Command: "evaluate", StopEpoch: started.StopEpoch, Expression: "x"}
	if _, err := m.Execute(t.Context(), evaluate); permission.Failure(err) == nil {
		t.Fatal("evaluation capability bypass")
	}
}
