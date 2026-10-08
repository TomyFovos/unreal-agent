//go:build linux || darwin

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeBridgePermissionDenialPreservesHistoryAndResumesWithoutEffects(t *testing.T) {
	// The shape is from SDKPermissionDeniedMessage; all values are synthetic.
	const denial = `{"type":"system","subtype":"permission_denied","tool_name":"mcp__unreal__unreal_read","tool_use_id":"private-call-sensitive","decision_reason_type":"asyncAgent","decision_reason":"private-policy-sensitive","message":"private-token-sensitive account-sensitive@example.invalid\u001b[31m","uuid":"private-uuid-sensitive","session_id":"private-session-sensitive"}`
	for _, tc := range []struct {
		name, want string
		fixture    testclaude.Config
		calls      int
	}{
		{"SDK denial", "provider permission denied", testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeEvent: denial + "\n" + denial + "\n" + `{"type":"result","subtype":"success","result":"must not become success"}`}, 1},
		{"inactive managed allowlist", "organization requires managed permission rules", testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeManagedPermissionsOnly: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testclaude.New(t)
			f.Set(t, tc.fixture)
			var fallback atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallback.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			cfg := multiConfiguration(t, f, server.URL, false)
			cfg.Runtime.ClaudeCode.ToolBridge.Enabled = true
			cfg.Permissions.Tools = []string{"read"}
			directory := privateCLIDirectory(t)
			c, shutdown, output := startClaudeStreamHost(t, cfg, directory)
			v, err := c.Open(t.Context(), host.Create, "permission-smoke")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, c, v, "original-request", "Inspect with Unreal tools; do not edit.")
			failed := inspectInteractive(t, c, "permission-smoke", func(v host.View) bool { return v.Failure != "" && !v.Running })
			if !strings.Contains(failed.Failure, tc.want) || strings.Contains(failed.Failure, "system_unknown_shape") || strings.Contains(failed.Failure, "sensitive") || responseCount(failed) != 0 || len(failed.Operations) != 0 || fallback.Load() != 0 || len(f.Calls(t)) != tc.calls {
				t.Fatal("denial leaked, became success, executed work, or fell back", failed.Failure)
			}
			count := func(v host.View) (int, []session.Turn) {
				inputs := 0
				var turns []session.Turn
				for _, item := range v.History.Items {
					switch data := item.Data.(type) {
					case inbox.Input:
						if data.Kind == inbox.InputExternal {
							inputs++
						}
					case session.Turn:
						turns = append(turns, data)
					}
				}
				return inputs, turns
			}
			inputs, turns := count(failed)
			if inputs != 1 || len(turns) != 1 {
				t.Fatal("duplicate event duplicated turn/input", inputs, len(turns))
			}
			snapshot := tui.Snapshot{ID: "permission-smoke", Failure: failed.Failure, Status: "failed", Connected: true}
			topology := tui.BuildOrchestration(snapshot, viewer.PanelSnapshot{}, time.Now(), 128)
			if len(topology.Nodes) != 1 || topology.Nodes[0].Problem != "provider permission denied" {
				t.Fatal("Orchestration lost safe provider status", topology.Nodes)
			}
			shutdown()
			// A later administrator approval / provider recovery can resume the
			// ORIGINAL input. No startup config change or side effect replay.
			f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog})
			c2, _, _ := startClaudeStreamHost(t, cfg, directory)
			if _, err = c2.Open(t.Context(), host.Resume, "permission-smoke"); err != nil {
				t.Fatal(err)
			}
			recovered := inspectInteractive(t, c2, "permission-smoke", func(v host.View) bool { return hasBridgeFinal(v) })
			inputs, recoveredTurns := count(recovered)
			if inputs != 1 || len(recoveredTurns) != 2 || recoveredTurns[1].PreviousTurnID != turns[0].ID || recoveredTurns[1].RuntimeRevision != turns[0].RuntimeRevision || len(recovered.Operations) != 0 || fallback.Load() != 0 || len(f.Calls(t)) != tc.calls+1 {
				t.Fatal("resume changed ownership, duplicated input, or replayed side effects")
			}
			if len(recovered.History.Items) < len(failed.History.Items) || !reflect.DeepEqual(recovered.History.Items[:len(failed.History.Items)], failed.History.Items) {
				t.Fatal("resume rewrote canonical history")
			}
			assertNoStoredCredentials(t, filepath.Join(directory, "sessions"), "private-call-sensitive", "private-policy-sensitive", "private-token-sensitive", "account-sensitive", "private-uuid-sensitive", "private-session-sensitive", "must not become success")
			assertNoCredentialLeak(t, output.String(), "sensitive")
		})
	}
}
