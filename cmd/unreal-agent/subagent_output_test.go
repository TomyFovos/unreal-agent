//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json/v2"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestSubagentListAndShowFlags(t *testing.T) {
	var view host.View
	for _, status := range []operation.Status{operation.StatusReady, operation.StatusAwaiting, operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: "parent", Template: "worker", Text: "original task", Configuration: &subagent.Template{Workspace: t.TempDir(), Runtime: []byte(`{}`)}})
		if err != nil {
			t.Fatal(err)
		}
		view.Operations = append(view.Operations, operation.Operation{ID: operation.ID("handle-" + string(status)), ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: status})
	}
	socket := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[host.View]
	current.Store(&view)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/inspect" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected request", 500)
			return
		}
		data, _ := json.Marshal(map[string]any{"View": current.Load()})
		w.Write(append(data, '\n'))
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	t.Setenv(subagentSocketEnv, socket)
	t.Setenv(subagentSessionEnv, "parent")
	for _, tc := range []struct {
		name string
		args []string
		want []string
		omit []string
	}{
		{"active", []string{"list"}, []string{"handle-ready", "handle-awaiting", "ended: 1 completed, 1 failed, 1 canceled (subagent list --all)"}, []string{"handle-completed", "handle-failed", "handle-canceled"}},
		{"all", []string{"list", "--all"}, []string{"handle-ready", "handle-awaiting", "handle-completed", "handle-failed", "handle-canceled"}, []string{"ended:"}},
		{"report", []string{"show", "handle-completed"}, []string{"handle-completed"}, []string{"task:", "original task"}},
		{"task-after", []string{"show", "handle-completed", "--task"}, []string{"task: original task"}, nil},
		{"task-before", []string{"show", "--task", "handle-completed"}, []string{"task: original task"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runSubagentCLI(t.Context(), tc.args, strings.NewReader(""), &out); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %q", want, out.String())
				}
			}
			for _, omit := range tc.omit {
				if strings.Contains(out.String(), omit) {
					t.Errorf("unexpected %q in %q", omit, out.String())
				}
			}
		})
	}
	current.Store(&host.View{Operations: view.Operations[2:]})
	var out bytes.Buffer
	if err := runSubagentCLI(t.Context(), []string{"list"}, nil, &out); err != nil || out.String() != "ended: 1 completed, 1 failed, 1 canceled (subagent list --all)\n" {
		t.Fatalf("ended-only list = %q, %v", out.String(), err)
	}
	current.Store(&host.View{})
	for _, args := range [][]string{{"list"}, {"list", "--all"}} {
		out.Reset()
		if err := runSubagentCLI(t.Context(), args, nil, &out); err != nil || out.String() != "No children in this Session.\n" {
			t.Fatalf("empty list = %q, %v", out.String(), err)
		}
	}
}

func TestSubagentReportRetainsResultsAndApprovals(t *testing.T) {
	child := childState{handle: "handle", task: "original task", approvals: []subagent.Approval{{ID: "request", Tool: "Bash", Scope: "command"}}, finish: &sessionstore.FinishRecord{Result: sessionstore.FinishResult{Status: "completed", Summary: "result details", ChangedFiles: []string{"file.go"}, Tests: []string{"focused: PASS"}, Blockers: []string{"parent decision"}}}}
	var out bytes.Buffer
	child.report(&out, "unreal-agent", false)
	for _, want := range []string{"approval needed: request request: Bash command", "result: completed", "summary: result details", "changed files:\n  - file.go", "tests:\n  - focused: PASS", "blockers:\n  - parent decision"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "original task") {
		t.Fatal("report repeated the task")
	}
}

func TestFirstLinePreservesUTF8(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 161), strings.Repeat("日", 60), strings.Repeat("a", 155) + "😀続き", strings.Repeat("a", 156) + "日続き"} {
		got := firstLine(text + "\nsecond line")
		if !utf8.ValidString(got) || len(got) > 160 || !strings.HasSuffix(got, "...") || !strings.HasPrefix(text, strings.TrimSuffix(got, "...")) {
			t.Errorf("invalid truncation: %q", got)
		}
	}
	if got := firstLine("  短い行\nsecond line"); got != "短い行" {
		t.Errorf("first line = %q", got)
	}
}
