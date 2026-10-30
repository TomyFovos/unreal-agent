package subagent_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestStoppedLeadHelper(t *testing.T) {
	if os.Getenv("UNREAL_STOPPED_LEAD_HELPER") != "1" {
		return
	}
	binary, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	err = sub.Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, c sub.ChildConfig, link sub.ChildLink) (*host.Session, io.Closer, error) {
		started := false
		model := adapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
			if c.Task != "lead" {
				return llm.Response{}, nil
			}
			if !started {
				started = true
				return response("SubagentStart", `{"template":"worker","task":"wait"}`), nil
			}
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), "fail lead now") {
				return llm.Response{}, errors.New("synthetic lead failure")
			}
			return llm.Response{}, nil
		})
		cfg := sub.Config{Directory: c.SessionDirectory, Binary: binary, Arguments: []string{"-test.run=^TestStoppedLeadHelper$"}, Environment: os.Environ(), Templates: c.Subagents, Child: &c, SendParent: link.Send}
		h, err := host.New(ctx, host.Config{Directory: c.SessionDirectory, Build: factory(cfg, model)})
		if err != nil {
			return nil, nil, err
		}
		raw, _ := json.Marshal(c)
		s, err := h.Open(ctx, host.Options{ID: c.ChildID, Lifecycle: "child", Configuration: raw, Policy: permission.Unrestricted(), Initial: []inbox.Input{c.InitialInput()}})
		if err != nil {
			h.Close()
			return nil, nil, err
		}
		// Rename so the test never reads a created but still empty pid file.
		pidPath := filepath.Join(c.SessionDirectory, string(c.ChildID)+".pid")
		if err = os.WriteFile(pidPath+".tmp", []byte(strconv.Itoa(os.Getpid())), 0600); err == nil {
			err = os.Rename(pidPath+".tmp", pidPath)
		}
		if err != nil {
			h.Close()
			return nil, nil, err
		}
		return s, h, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

func TestStoppedLeadCancelsGrandchildAndReapsProcess(t *testing.T) {
	for _, mode := range []string{"failure", "cancel", "host-close", "crash"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			worker := sub.Template{Workspace: dir, Runtime: jsontext.Value(`{"Provider":{"provider":"fake","model":{"id":"fixture"}},"ReasoningEffort":"medium"}`), Policy: permission.Config{Tools: []string{"Finish"}}}
			lead := worker
			lead.Policy.Tools = []string{"Finish", "SubagentStart"}
			lead.Policy.ProcessMode = permission.ProcessUnrestricted
			lead.Subagents = map[string]sub.Template{"worker": worker}
			cfg := sub.Config{Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestStoppedLeadHelper$"}, Environment: append(os.Environ(), "UNREAL_STOPPED_LEAD_HELPER=1"), Templates: map[string]sub.Template{"lead": lead}}
			model := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
				return llm.Response{}, nil
			})
			h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(cfg, model)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { h.Close() })
			parent, err := h.Create(t.Context(), host.Options{ID: "main", Policy: permission.Unrestricted()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = sub.StartChild(t.Context(), parent, parent.Generation, "start-lead", "lead", "lead", cfg.Templates, nil, nil); err != nil {
				t.Fatal(err)
			}
			leadOp := awaitChild(t, parent, false)
			plan, err := sub.DecodePlan(leadOp)
			if err != nil {
				t.Fatal(err)
			}
			var grandchild operation.Operation
			var grandPlan sub.Plan
			awaitBoundary(t, func() bool {
				parentView, e := parent.Inspect(0, 256)
				if e != nil {
					t.Fatal(e)
				}
				for _, op := range parentView.Operations {
					if op.ID == leadOp.ID && (op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled) {
						state, _ := operation.DecodeRemoteJobState(op)
						t.Fatalf("lead stopped before worker startup: %s: %s", op.Status, state.TerminalError)
					}
				}
				v, e := sub.ReadChildSnapshot(t.Context(), dir, plan.ChildID)
				if e != nil {
					return false
				}
				for _, op := range v.View.Operations {
					p, e := sub.DecodePlan(op)
					if e == nil && p.Action == "start" {
						if _, e = os.Stat(filepath.Join(dir, string(p.ChildID)+".pid")); e == nil {
							grandchild, grandPlan = op, p
							return true
						}
					}
				}
				return false
			})
			if grandchild.Status != operation.StatusAwaiting {
				t.Fatalf("live grandchild=%+v", grandchild)
			}
			data, err := os.ReadFile(filepath.Join(dir, string(grandPlan.ChildID)+".pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil || syscall.Kill(pid, 0) != nil {
				t.Fatal("grandchild process not running", err)
			}
			control := sub.ControlRequest{ParentID: parent.ID, ParentGeneration: parent.Generation, OperationID: leadOp.ID, ChildID: plan.ChildID, InputID: "stop-lead"}
			switch mode {
			case "failure":
				control.Text = "fail lead now"
				_, err = sub.SteerChild(t.Context(), parent, control)
			case "cancel":
				_, err = sub.CancelChild(t.Context(), parent, control)
			case "host-close":
				err = h.Close()
			case "crash":
				data, err = os.ReadFile(filepath.Join(dir, string(plan.ChildID)+".pid"))
				if err == nil {
					var leadPID int
					leadPID, err = strconv.Atoi(string(data))
					if err == nil {
						err = syscall.Kill(leadPID, syscall.SIGKILL)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != "host-close" {
				ended := awaitChild(t, parent, true)
				want := operation.StatusFailed
				if mode == "cancel" {
					want = operation.StatusCanceled
				}
				if ended.Status != want {
					t.Fatalf("lead status=%s, want %s", ended.Status, want)
				}
			}
			store, err := localfile.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			ops, err := store.Operations(t.Context(), plan.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, op := range ops {
				if op.ID == grandchild.ID {
					found = true
					if op.Status != operation.StatusCanceled {
						t.Fatalf("stopped lead left grandchild %s", op.Status)
					}
				}
			}
			if !found {
				t.Fatal("grandchild operation disappeared")
			}
			awaitBoundary(t, func() bool {
				// A reparented process can briefly remain a zombie awaiting PID 1.
				data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				return errors.Is(e, os.ErrNotExist) || e == nil && strings.Contains(string(data), ") Z ")
			})
			lock, err := store.AcquireWriter(grandPlan.ChildID)
			if err != nil {
				t.Fatal("grandchild kept execution ownership", err)
			}
			lock.Close()
			projection := viewer.New(viewer.SubagentOptions())
			view, err := sub.ReadChildSnapshot(t.Context(), dir, plan.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			if err = projection.Replace(plan.ChildID, view.View); err != nil {
				t.Fatal(err)
			}
			for _, row := range projection.Rows(time.Now()) {
				if row.ID == grandPlan.ChildID && (row.ParentOperationStatus != operation.StatusCanceled || row.ParentRuntime == viewer.RuntimeRunning) {
					t.Fatalf("viewer kept grandchild active: %+v", row)
				}
			}
		})
	}
}
