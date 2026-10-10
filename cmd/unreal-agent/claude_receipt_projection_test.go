//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// Real Builder -> production adapter -> native fake CLI -> native executor.
// The fake model derives every edit argument solely from its received read.
func TestClaudeStructuredReceiptProjectionMiddleEdit(t *testing.T) {
	for _, tc := range []struct {
		size    int
		partial bool
	}{{128, false}, {4096, false}, {8192, false}, {12288, false}, {8192, true}} {
		t.Run(fmt.Sprintf("bytes=%d/range=%t", tc.size, tc.partial), func(t *testing.T) {
			f := testclaude.New(t)
			cfg := humanModeConfiguration(bridgeServeConfiguration(t, f))
			cfg.Context = contextengine.Config{InputBudget: 24576}
			cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
			cfg.Runtime.ClaudeCode.ToolBridge.ResultBytes = 65536
			body := strings.Repeat("x", tc.size/2) + "\nMIDDLE_EDIT_TARGET=before\n" + strings.Repeat("y", tc.size/2) + "\n"
			path := filepath.Join(cfg.Runtime.Workspace, "fixture.txt")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			readArgs := `{"path":"fixture.txt"}`
			if tc.partial {
				readArgs = `{"path":"fixture.txt","start_line":2,"lines":1}`
			}
			f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, BridgeSteps: []testclaude.BridgeStep{
				{Name: "read", Arguments: readArgs, Contains: "MIDDLE_EDIT_TARGET=before"},
				{Name: "edit", ReadEdit: true, Contains: "applied", Duplicate: true},
				{Name: "read", Arguments: readArgs, Contains: "MIDDLE_EDIT_TARGET=after"},
			}})
			client, _, _ := startInteractiveHost(t, cfg)
			v, err := client.Open(t.Context(), host.Create, "middle-edit")
			if err != nil {
				t.Fatal(err)
			}
			control, err := client.WatchPermissions(t.Context(), "middle-edit", v.Generation)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Cancel()
			setGatewayHumanMode(t, control, permission.FullAccess)
			submitInteractive(t, client, v, "fixture-task", "Read fixture.txt; replace its observed middle target with after, replay that exact edit, verify through read, and return Final.")
			end := inspectInteractive(t, client, "middle-edit", func(v host.View) bool {
				if v.Failure != "" {
					t.Fatal("fake structured continuation failed", v.Failure)
				}
				return hasBridgeFinal(v)
			})
			got, err := os.ReadFile(path)
			if err != nil || string(got) != strings.Replace(body, "MIDDLE_EDIT_TARGET=before", "MIDDLE_EDIT_TARGET=after", 1) || len(end.Operations) != 3 {
				t.Fatal("middle edit or at-most-once execution failed")
			}
			for _, op := range end.Operations {
				if op.Status != operation.StatusCompleted || op.Denial != nil {
					t.Fatal("actual file and canonical Operation disagree")
				}
			}
			calls := f.Calls(t)
			if len(calls) != 5 {
				t.Fatal("duplicate edit executed or final generation missing")
			}
			var observed files.Result
			const label = "\n\nLast Unreal Action Result (machine-owned canonical receipt; Failed=true means failure):\n"
			parts := strings.SplitN(calls[1].Input, label, 2)
			var outcome llm.ToolOutcome
			if len(parts) != 2 || json.Unmarshal([]byte(parts[1]), &outcome) != nil || len(outcome.Result.Output) != 1 || json.Unmarshal([]byte(outcome.Result.Output[0].Value), &observed) != nil || observed.Revision == nil {
				t.Fatal("fake CLI did not receive a complete typed native read")
			}
			want := body
			if tc.partial {
				want = "MIDDLE_EDIT_TARGET=before\n"
			}
			if observed.Content != want || observed.Truncated || (tc.partial && observed.StartLine != 2) || strings.Count(calls[1].Input, "MIDDLE_EDIT_TARGET=before") != 1 {
				t.Fatal("body/range/truncation altered or duplicate injection")
			}
			statuses, responses := 0, 0
			for _, item := range end.History.Items {
				if status, ok := item.Data.(sessionstore.ToolCallStatus); ok && len(status.Operations) == 1 && status.Operations[0].Status == operation.StatusCompleted {
					statuses++
					if len(status.Status.WaitingFor) != 1 {
						t.Fatal("canonical dependency erased")
					}
				}
				if r, ok := item.Data.(sessionstore.ModelResponse); ok {
					for _, output := range r.Response.Output {
						if m, ok := output.Data.(llm.Message); ok {
							responses++
							if m.Text != "Bridge fixture completed" {
								t.Fatal("private protocol became a public response")
							}
						}
					}
				}
			}
			if statuses != 3 || responses != 1 {
				t.Fatal("canonical receipt/final counts do not match execution")
			}
		})
	}
}

func TestClaudeStructuredReceiptProjectionSecretHost(t *testing.T) {
	f := testclaude.New(t)
	cfg := humanModeConfiguration(bridgeServeConfiguration(t, f))
	cfg.Runtime.ClaudeCode.ToolBridge.Mode = claudecode.BridgeModeStructured
	if os.WriteFile(filepath.Join(cfg.Runtime.Workspace, "protected.txt"), []byte("Authorization: Bearer SYNTHETIC_SECRET_BODY"), 0600) != nil {
		t.Fatal("private fixture creation failed")
	}
	f.Set(t, testclaude.Config{Subscription: "team", BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"protected.txt"}`, Contains: `"state":"completed"`}}})
	client, _, _ := startInteractiveHost(t, cfg)
	v, err := client.Open(t.Context(), host.Create, "protected-read")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, client, v, "task", "Read protected.txt and observe the Unreal redaction state, then finish.")
	end := inspectInteractive(t, client, "protected-read", func(v host.View) bool {
		if v.Failure != "" {
			t.Fatal("secret receipt failed continuation", v.Failure)
		}
		return hasBridgeFinal(v)
	})
	if len(end.Operations) != 1 || end.Operations[0].Status != operation.StatusCompleted {
		t.Fatal("redaction altered execution state")
	}
	calls := f.Calls(t)
	if len(calls) != 2 || strings.Contains(calls[1].Input, "SYNTHETIC_SECRET_BODY") {
		t.Fatal("secret leaked to the provider or generation count changed")
	}
	parts := strings.SplitN(calls[1].Input, "\n\nLast Unreal Action Result (machine-owned canonical receipt; Failed=true means failure):\n", 2)
	var outcome llm.ToolOutcome
	var stub struct {
		Redacted        bool     `json:"redacted"`
		ReceiptPresent  bool     `json:"receipt_present"`
		State           string   `json:"state"`
		OperationStates []string `json:"operation_states"`
	}
	if len(parts) != 2 || json.Unmarshal([]byte(parts[1]), &outcome) != nil || len(outcome.Result.Output) != 1 || json.Unmarshal([]byte(outcome.Result.Output[0].Value), &stub) != nil || !stub.Redacted || !stub.ReceiptPresent || stub.State != "completed" || len(stub.OperationStates) != 1 || stub.OperationStates[0] != "completed" || outcome.Failed {
		t.Fatal("final fake CLI input lost the actual-state redaction stub")
	}
	state, err := operation.DecodeRemoteJobState(end.Operations[0])
	if err != nil || !strings.Contains(string(state.Handle), "SYNTHETIC_SECRET_BODY") {
		t.Fatal("request redaction rewrote the canonical receipt")
	}
}
