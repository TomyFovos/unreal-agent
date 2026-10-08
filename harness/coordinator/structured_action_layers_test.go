//go:build linux || darwin

package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	nativetool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

// Inert provider proposals enter the actual Coordinator/Registry/Permission/
// Operation/Executor path. The only filesystem/shell effects are owned by Unreal
// in temporary fixtures. No CLI, auth, SDK or provider process exists here.
func TestStructuredLayerActionExecution(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("old value\n")))
	for _, tc := range []struct {
		name, tool, args, contains string
		toolDeny, pathDeny         bool
		operations                 int
		failed                     bool
	}{
		{"read", "read", `{"path":"fixture.txt"}`, "old value", false, false, 1, false},
		{"glob", "glob", `{"path":".","glob":"*.txt"}`, "fixture.txt", false, false, 1, false},
		{"grep", "grep", `{"path":".","pattern":"old value"}`, "old value", false, false, 1, false},
		{"write", "write", `{"path":"created.txt","expected":{"exists":false},"content":"created value"}`, "applied", false, false, 1, false},
		{"edit", "edit", `{"path":"fixture.txt","expected":{"exists":true,"digest":"` + digest + `"},"old_text":"old","new_text":"new"}`, "applied", false, false, 1, false},
		{"Bash-success", "Bash", `{"command":"printf 'command value'","max_output_length":1024}`, "command value", false, false, 1, false},
		{"Bash-failure", "Bash", `{"command":"printf 'test failure'; exit 1","max_output_length":1024}`, "Exit code: 1", false, false, 1, true},
		{"Bash-restricted", "Bash", `{"command":"printf 'should not run'"}`, "sandbox is unavailable", false, false, 1, true},
		{"edit-no-match", "edit", `{"path":"fixture.txt","expected":{"exists":true,"digest":"` + digest + `"},"old_text":"missing","new_text":"new"}`, "no_match", false, false, 1, true},
		{"malformed-arguments", "read", `{"path":true}`, "required string", false, false, 0, true},
		{"unknown-registry-tool", "unregistered", `{}`, "not available", false, false, 0, true},
		{"tool-deny", "read", `{"path":"fixture.txt"}`, "denied", true, false, 0, true},
		{"filesystem-deny", "read", `{"path":"fixture.txt"}`, "permission denied", false, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("old value\n"), 0600); err != nil {
				t.Fatal(err)
			}
			names := []string{"read", "glob", "grep", "write", "edit", "Bash", "unregistered"}
			if tc.toolDeny {
				names = nil
			}
			reads := []string{root}
			if tc.pathDeny {
				reads = nil
			}
			config := permission.Config{Tools: names, ReadRoots: reads, WriteRoots: []string{root}, ProcessMode: permission.ProcessUnrestricted}
			// These two harmless printf fixtures explicitly grant a local test
			// process profile. Production defaults are untouched; the restricted
			// case above verifies that unavailable sandboxing remains fail closed.
			if tc.tool == "Bash" && tc.name != "Bash-restricted" {
				config.FilesystemUnrestricted, config.NetworkUnrestricted = true, true
			}
			policy, err := permission.New(config)
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			ctx, cancel := context.WithCancel(permission.WithPolicy(t.Context(), policy))
			defer cancel()
			service, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, path string, write bool) error {
				return permission.FromContext(ctx).CheckPath(path, write)
			}})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := files.NewHandler(ctx, files.Executor{Files: service}, "action-unit")
			if err != nil {
				t.Fatal(err)
			}
			defer handler.Close()
			manager := operation.NewLocalOperationManagerWithPolicy(ctx, policy, handler)
			registry := tool.NewRegistry(nativetool.Configure(tool.StaticTranslators{Bash: bash.New(bash.Config{Shell: "/bin/sh", BaseDirectory: root})}), tool.ReadName, tool.GlobName, tool.GrepName, tool.WriteName, tool.EditName, tool.BashName)
			store := emptyFakeStore()
			current := newTestCoordinator(store, newTestInbox(t), newFakeOperationManager(), contextbuilder.NewBuilder(), registry)
			current.dependencies.Operations = manager
			item, err := current.addItemToLocalState(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "origin", Type: session.TurnRegular, RuntimeRevision: 7}})
			if err != nil || current.storeItemInSessionStore(ctx, item) != nil {
				t.Fatal("canonical turn setup", err)
			}
			current.cancelModel = func() {}
			current.modelOrigin, current.modelRevision = "origin", 7
			call := llm.ToolCall{CallID: "structured-unit-action", Name: tc.tool, Arguments: tc.args}
			request := bridgeRequest{ctx: ctx, origin: "origin", reply: make(chan bridgeReply, 1), response: llm.Response{ID: "unit-generation", Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}
			if err := current.acceptBridgeRequest(ctx, request); err != nil {
				t.Fatal(err)
			}
			if _, err := current.processEvents(ctx); err != nil {
				t.Fatal(err)
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			var reply bridgeReply
		waiting:
			for {
				select {
				case reply = <-request.reply:
					break waiting
				case update := <-manager.Updates():
					if err := current.handleOperationUpdate(ctx, update); err != nil {
						t.Fatal(err)
					}
					if _, err := current.processEvents(ctx); err != nil {
						t.Fatal(err)
					}
				case <-deadline.C:
					t.Fatal("canonical receipt not delivered")
				}
			}
			if reply.err != nil || len(reply.outcomes) != 1 || reply.outcomes[0].Failed != tc.failed {
				t.Fatal("typed canonical outcome", reply.err, reply.outcomes)
			}
			var output strings.Builder
			for _, block := range reply.outcomes[0].Result.Output {
				output.WriteString(block.Value)
			}
			if !strings.Contains(output.String(), tc.contains) {
				t.Fatal("canonical executor feedback missing", output.String())
			}
			if len(current.state.operations) != tc.operations || len(store.appendedResponses) != 1 {
				t.Fatal("Operation/public call count", len(current.state.operations), len(store.appendedResponses))
			}
			for _, op := range current.state.operations {
				if !operationIsTerminal(op.Status) || op.ToolName != tc.tool {
					t.Fatal("Operation identity/lifecycle")
				}
			}
			if tc.pathDeny {
				for _, op := range current.state.operations {
					if op.Denial == nil {
						t.Fatal("filesystem denial lost")
					}
				}
				if strings.Contains(output.String(), "old value") {
					t.Fatal("denied file content leaked")
				}
			}
			content, err := os.ReadFile(filepath.Join(root, "fixture.txt"))
			want := "old value\n"
			if tc.name == "edit" {
				want = "new value\n"
			}
			if err != nil || string(content) != want {
				t.Fatal("unexpected side effect", err)
			}
			if tc.name == "write" {
				content, err = os.ReadFile(filepath.Join(root, "created.txt"))
				if err != nil || string(content) != "created value" {
					t.Fatal("create not executed via Operation", err)
				}
			}
			encoded, _ := json.Marshal(store.appendedResponses)
			if strings.Contains(string(encoded), "StructuredOutput") || strings.Contains(string(encoded), `\"type\":\"action\"`) {
				t.Fatal("provider protocol entered canonical conversation")
			}
			// Replay uses the canonical receipt index, never executes a second time.
			replay := request
			replay.reply = make(chan bridgeReply, 1)
			if err := current.acceptBridgeRequest(ctx, replay); err != nil {
				t.Fatal(err)
			}
			got := <-replay.reply
			if got.err != nil || len(current.state.operations) != tc.operations || len(store.appendedResponses) != 1 {
				t.Fatal("receipt replay executed again", got.err)
			}
		})
	}
}
