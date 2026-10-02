package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestLanguageServerProcess(t *testing.T) {
	if os.Getenv("UNREAL_LSP_TEST_SERVER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	documents := map[string]string{}
	versions := map[string]int{}
	write := func(message any) {
		data, _ := json.Marshal(message)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n", len(data))
		_, _ = os.Stdout.Write(data)
	}
	response := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	for {
		length := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				os.Exit(0)
			}
			if line == "\r\n" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				length, _ = strconv.Atoi(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
			}
		}
		if length < 1 || length > maxMessageBytes {
			os.Exit(7)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			os.Exit(8)
		}
		var msg envelope
		if json.Unmarshal(data, &msg) != nil {
			os.Exit(9)
		}
		var params struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Text    string `json:"text"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
			Position Position `json:"position"`
			Query    string   `json:"query"`
			NewName  string   `json:"newName"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		uri := params.TextDocument.URI
		switch msg.Method {
		case "initialize":
			encoding := os.Getenv("UNREAL_LSP_TEST_ENCODING")
			if encoding == "" {
				encoding = "utf-16"
			}
			response(msg.ID, map[string]any{"capabilities": map[string]any{"positionEncoding": encoding, "textDocumentSync": 2, "definitionProvider": true, "referencesProvider": true, "hoverProvider": true, "documentSymbolProvider": true, "workspaceSymbolProvider": true, "renameProvider": true, "codeActionProvider": true}})
		case "initialized", "$/cancelRequest":
		case "textDocument/didOpen", "textDocument/didChange":
			text := params.TextDocument.Text
			if len(params.ContentChanges) > 0 {
				text = params.ContentChanges[0].Text
			}
			documents[uri] = text
			versions[uri] = params.TextDocument.Version
			write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": uri, "version": versions[uri], "diagnostics": []any{map[string]any{"range": Range{}, "message": "example", "severity": 2}}}})
		case "textDocument/hover":
			response(msg.ID, map[string]any{"contents": map[string]any{"kind": "plaintext", "value": documents[uri]}, "observedPosition": params.Position, "version": versions[uri]})
		case "textDocument/definition", "textDocument/references":
			response(msg.ID, []any{map[string]any{"uri": uri, "range": Range{Position{0, 0}, Position{0, 3}}}})
		case "textDocument/documentSymbol":
			response(msg.ID, []any{map[string]any{"name": "old", "kind": 12, "range": Range{}, "selectionRange": Range{}}})
		case "workspace/symbol":
			if params.Query == "crash" {
				os.Exit(4)
			}
			if params.Query == "slow" {
				time.Sleep(250 * time.Millisecond)
			}
			if params.Query == "large" {
				response(msg.ID, strings.Repeat("x", MaxResultBytes+1))
			} else {
				response(msg.ID, []any{map[string]any{"name": "symbol", "kind": 12}})
			}
		case "textDocument/rename":
			if params.NewName == "create" {
				directory, _ := os.Getwd()
				target := fileURI(filepath.Join(directory, "created.go"))
				response(msg.ID, map[string]any{"documentChanges": []any{
					map[string]any{"kind": "create", "uri": target},
					map[string]any{"textDocument": map[string]any{"uri": target, "version": nil}, "edits": []any{map[string]any{"range": Range{}, "newText": "created"}}},
				}})
				continue
			}
			if params.NewName == "delete" {
				response(msg.ID, map[string]any{"documentChanges": []any{map[string]any{"kind": "delete", "uri": uri}}})
				continue
			}
			keys := make([]string, 0, len(documents))
			for key := range documents {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			changes := []any{}
			for _, key := range keys {
				if !strings.HasSuffix(key, ".go") || !strings.HasPrefix(documents[key], "old") {
					continue
				}
				version := versions[key]
				if params.NewName == "badversion" {
					version++
				}
				changes = append(changes, map[string]any{"textDocument": map[string]any{"uri": key, "version": version}, "edits": []any{map[string]any{"range": Range{Position{0, 0}, Position{0, 3}}, "newText": params.NewName}}})
			}
			if params.NewName == "stale" {
				_ = os.WriteFile("b.go", []byte("external"), 0600)
			}
			response(msg.ID, map[string]any{"documentChanges": changes})
		case "textDocument/codeAction":
			response(msg.ID, []any{
				map[string]any{"title": "safe", "kind": "quickfix", "edit": map[string]any{"changes": map[string]any{uri: []any{map[string]any{"range": Range{Position{0, 0}, Position{0, 3}}, "newText": "new"}}}}},
				map[string]any{"title": "unsafe", "command": map[string]any{"command": "touch bad", "arguments": []any{}}},
				map[string]any{"title": "needs resolve", "data": map[string]any{"id": "opaque"}},
				map[string]any{"title": "delete", "edit": map[string]any{"documentChanges": []any{map[string]any{"kind": "delete", "uri": uri}}}},
			})
		case "shutdown":
			response(msg.ID, nil)
		case "exit":
			os.Exit(0)
		default:
			if len(msg.ID) > 0 {
				write(envelope{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{-32601, "method missing"}})
			}
		}
	}
}

func newTestManager(t *testing.T, encoding string) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("old value\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(permission.WithPolicy(t.Context(), permission.Unrestricted()), Config{Mutation: service, RequestTimeout: 3 * time.Second, Servers: []ServerConfig{{
		Language: "go", Path: executable, Arguments: []string{"-test.run=^TestLanguageServerProcess$"}, Environment: append(os.Environ(), "UNREAL_LSP_TEST_SERVER=1", "UNREAL_LSP_TEST_ENCODING="+encoding), Extensions: []string{".go"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	return manager, root
}
func execute(t *testing.T, manager *Manager, request Request) Result {
	t.Helper()
	result := manager.Execute(t.Context(), "session/"+t.Name()+"/"+request.Action, request)
	if result.Code != "ok" && result.Code != "applied" {
		t.Fatalf("result=%+v", result)
	}
	return result
}

func TestActualServerInitializeReuseSemanticOperationsAndShutdown(t *testing.T) {
	manager, root := newTestManager(t, "utf-16")
	var generation string
	for _, action := range []string{"definition", "references", "document_symbols", "hover", "diagnostics", "workspace_symbols"} {
		result := execute(t, manager, Request{Language: "go", Action: action, Path: "a.go"})
		if generation != "" && generation != result.Generation {
			t.Fatal("server was not reused")
		}
		generation = result.Generation
		if !json.Valid(result.Data) {
			t.Fatalf("invalid typed response=%s", result.Data)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	result := execute(t, manager, Request{Language: "go", Action: "hover", Path: "a.go"})
	var hover struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(result.Data, &hover)
	if hover.Version != 2 {
		t.Fatalf("document version=%d", hover.Version)
	}
	stopped := execute(t, manager, Request{Language: "go", Action: "shutdown", Generation: generation})
	if stopped.Generation != generation {
		t.Fatal("wrong shutdown generation")
	}
	result = manager.Execute(t.Context(), "stale", Request{Language: "go", Action: "shutdown", Generation: generation})
	if result.Code != "expired" {
		t.Fatalf("stale control=%+v", result)
	}
	fresh := execute(t, manager, Request{Language: "go", Action: "hover", Path: "a.go"})
	if fresh.Generation == generation {
		t.Fatal("shutdown handle was reused")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, server := range manager.servers {
		select {
		case <-server.rpc.done:
		default:
			t.Fatal("server process did not exit")
		}
	}
}

func TestServerCrashRestartAndStaleGeneration(t *testing.T) {
	manager, _ := newTestManager(t, "utf-16")
	initial := execute(t, manager, Request{Language: "go", Action: "hover", Path: "a.go"})
	crashed := manager.Execute(t.Context(), "crash", Request{Language: "go", Action: "workspace_symbols", Query: "crash"})
	if crashed.Code != "unavailable" {
		t.Fatalf("crash=%+v", crashed)
	}
	old := manager.Execute(t.Context(), "old", Request{Language: "go", Action: "hover", Path: "a.go", Generation: initial.Generation})
	if old.Code != "expired" {
		t.Fatalf("old=%+v", old)
	}
	fresh := execute(t, manager, Request{Language: "go", Action: "hover", Path: "a.go"})
	restarted := execute(t, manager, Request{Language: "go", Action: "restart", Generation: fresh.Generation})
	if restarted.Generation == fresh.Generation || restarted.Generation == initial.Generation {
		t.Fatal("restart did not invalidate generation")
	}
}

func TestServerPositionEncodingAndInvalidUnicodeBoundary(t *testing.T) {
	manager, root := newTestManager(t, "utf-8")
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("a\U0001F600z\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result := execute(t, manager, Request{Language: "go", Action: "hover", Path: "a.go", Position: Position{0, 3}})
	var hover struct {
		Position Position `json:"observedPosition"`
	}
	_ = json.Unmarshal(result.Data, &hover)
	if result.Encoding != "utf-8" || hover.Position.Character != 5 {
		t.Fatalf("position=%+v result=%+v", hover, result)
	}
	invalid := manager.Execute(t.Context(), "invalid", Request{Language: "go", Action: "hover", Path: "a.go", Position: Position{0, 2}})
	if invalid.Code != "invalid" {
		t.Fatalf("surrogate split=%+v", invalid)
	}
}

func TestRenamePreflightAllTargetsAndVersionChecks(t *testing.T) {
	for _, name := range []string{"renamed", "stale", "badversion", "delete"} {
		t.Run(name, func(t *testing.T) {
			manager, root := newTestManager(t, "utf-16")
			result := manager.Execute(t.Context(), "rename-"+name, Request{Language: "go", Action: "rename", Path: "a.go", NewName: name})
			a, _ := os.ReadFile(filepath.Join(root, "a.go"))
			b, _ := os.ReadFile(filepath.Join(root, "b.go"))
			switch name {
			case "renamed":
				if result.Code != "applied" || string(a) != "renamed value\n" || string(b) != "renamed value\n" {
					t.Fatalf("result=%+v a=%s b=%s", result, a, b)
				}
			case "stale":
				if result.Code != "stale" || string(a) != "old value\n" || string(b) != "external" {
					t.Fatalf("partial stale application: result=%+v a=%s b=%s", result, a, b)
				}
			case "badversion":
				if result.Code != "stale" || string(a) != "old value\n" {
					t.Fatalf("bad version=%+v", result)
				}
			case "delete":
				if result.Code != "unsupported" || string(a) != "old value\n" {
					t.Fatalf("unsupported resource mutated: %+v", result)
				}
			}
		})
	}
}
func TestCreateResourceUsesAbsentPrecondition(t *testing.T) {
	manager, root := newTestManager(t, "utf-16")
	result := execute(t, manager, Request{Language: "go", Action: "rename", Path: "a.go", NewName: "create"})
	if result.Mutation == nil || result.Mutation.Code != mutation.Applied {
		t.Fatalf("create=%+v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "created.go"))
	if err != nil || string(data) != "created" {
		t.Fatalf("created=%q %v", data, err)
	}
}

func TestCodeActionsRejectCommandsAndRevalidateSnapshots(t *testing.T) {
	manager, root := newTestManager(t, "utf-16")
	result := execute(t, manager, Request{Language: "go", Action: "code_actions", Path: "a.go"})
	var actions []ActionSummary
	if err := json.Unmarshal(result.Data, &actions); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 4 || actions[0].ID == "" || actions[0].Unsupported {
		t.Fatalf("actions=%+v", actions)
	}
	for _, action := range actions[1:] {
		if !action.Unsupported || action.ID != "" {
			t.Fatalf("unsafe action accepted: %+v", action)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := manager.Execute(t.Context(), "action", Request{Language: "go", Action: "apply_code_action", ActionID: actions[0].ID})
	if stale.Code != "stale" {
		t.Fatalf("action=%+v", stale)
	}
	again := manager.Execute(t.Context(), "action-again", Request{Language: "go", Action: "apply_code_action", ActionID: actions[0].ID})
	if again.Code != "expired" {
		t.Fatalf("action replay=%+v", again)
	}
}

func TestMissingServerPermissionFailuresAndBoundedResults(t *testing.T) {
	manager, _ := newTestManager(t, "utf-16")
	missing := manager.Execute(t.Context(), "missing", Request{Language: "missing", Action: "workspace_symbols"})
	if missing.Code != "unavailable" {
		t.Fatalf("missing=%+v", missing)
	}
	denied := manager.Execute(permission.WithPolicy(t.Context(), permission.DenyAll()), "denied", Request{Language: "go", Action: "hover", Path: "a.go"})
	if denied.Code != "denied" || denied.Denial == nil || len(manager.servers) != 0 {
		t.Fatalf("denial=%+v", denied)
	}
	policy, err := permission.New(permission.Config{Tools: []string{"LSP"}, ProcessMode: permission.ProcessUnrestricted})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	unsupported := manager.Execute(permission.WithPolicy(t.Context(), policy), "restricted", Request{Language: "go", Action: "hover", Path: "a.go"})
	if unsupported.Code != "unsupported" || len(manager.servers) != 0 {
		t.Fatalf("sandbox=%+v", unsupported)
	}
	large := manager.Execute(t.Context(), "large", Request{Language: "go", Action: "workspace_symbols", Query: "large"})
	if large.Code != "limit" || len(large.Data) != 0 {
		t.Fatalf("large=%+v", large)
	}
}
func TestRequestCancellationAndLaterReuse(t *testing.T) {
	manager, _ := newTestManager(t, "utf-16")
	execute(t, manager, Request{Language: "go", Action: "workspace_symbols"})
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	result := manager.Execute(ctx, "slow", Request{Language: "go", Action: "workspace_symbols", Query: "slow"})
	if result.Code != "canceled" {
		t.Fatalf("cancellation=%+v", result)
	}
	execute(t, manager, Request{Language: "go", Action: "workspace_symbols"})
}

func TestFrameAndEditValidation(t *testing.T) {
	for _, input := range []string{"Content-Length: -1\r\n\r\n", "Content-Length: 1\r\nContent-Length: 2\r\n\r\n", "Wrong\r\n\r\n", strings.Repeat("x", 8193)} {
		if _, _, err := frame([]byte(input)); err == nil {
			t.Fatalf("accepted frame=%q", input)
		}
	}
	if _, n, err := frame([]byte("Content-Length: 4\r\n\r\n{}")); n != 0 || err != nil {
		t.Fatal("partial frame treated as complete")
	}
	data := []byte("a\U0001F600z\r\nnext")
	for _, encoding := range []string{"utf-8", "utf-16", "utf-32"} {
		for _, offset := range []int{0, 1, 5, 6, 8, 12} {
			pos := offsetPosition(data, offset, encoding)
			got, err := positionOffset(data, pos, encoding)
			if err != nil || got != offset {
				t.Fatalf("roundtrip %s offset=%d got=%d err=%v", encoding, offset, got, err)
			}
		}
	}
	edits := []textEdit{{Range: Range{Position{0, 0}, Position{0, 2}}, NewText: "a"}, {Range: Range{Position{0, 1}, Position{0, 3}}, NewText: "b"}}
	if _, err := replaceText([]byte("abcd"), edits, "utf-16"); err == nil {
		t.Fatal("overlapping edits accepted")
	}
	edits = []textEdit{{Range: Range{}, NewText: "a"}, {Range: Range{}, NewText: "b"}}
	result, err := replaceText([]byte("c"), edits, "utf-16")
	if err != nil || string(result) != "abc" {
		t.Fatalf("equal-position insertion order=%q %v", result, err)
	}
}

type inertContext struct{ specs []operation.Spec }

func (c *inertContext) Submit(spec operation.Spec) operation.ID {
	c.specs = append(c.specs, spec)
	return "lsp-operation"
}
func TestPureTranslatorDurableHandlerAndInterruptedMutation(t *testing.T) {
	ctx := &inertContext{}
	translator := Translator{}
	status := translator.Translate(ctx, llm.ToolCall{Name: ToolName, Arguments: `{"language":"go","action":"hover","path":"a.go"}`})
	if status.Error != "" || len(ctx.specs) != 1 {
		t.Fatalf("translation=%+v", status)
	}
	manager, _ := newTestManager(t, "utf-16")
	handler, err := NewHandler(t.Context(), manager, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	spec := ctx.specs[0]
	current := operation.Operation{ID: "hover", ToolName: ToolName, Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, MaxOutputLength: spec.MaxOutputLength, State: spec.State}
	if err := handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	var completed operation.Operation
	select {
	case completed = <-handler.RemoteJobUpdates():
		if completed.Status == operation.StatusAwaiting {
			completed = <-handler.RemoteJobUpdates()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no LSP operation result")
	}
	result, err := translator.TranslateResult("call", status, []operation.Operation{completed})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != operation.StatusCompleted || !json.Valid([]byte(result.Output[0].Value)) {
		t.Fatalf("completed=%+v result=%+v", completed, result)
	}
	ctx = &inertContext{}
	status = translator.Translate(ctx, llm.ToolCall{Name: ToolName, Arguments: `{"language":"go","action":"rename","path":"a.go","new_name":"renamed"}`})
	spec = ctx.specs[0]
	resumed := operation.Operation{ID: "interrupted", ToolName: ToolName, Type: spec.Type, Version: spec.Version, Status: operation.StatusAwaiting, MaxOutputLength: spec.MaxOutputLength, State: spec.State}
	if err = handler.AddRemoteJob(resumed); err != nil {
		t.Fatal(err)
	}
	select {
	case completed = <-handler.RemoteJobUpdates():
		if completed.Status == operation.StatusAwaiting {
			completed = <-handler.RemoteJobUpdates()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no interrupted result")
	}
	state, err := operation.DecodeRemoteJobState(completed)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != operation.StatusFailed || !bytes.Contains(state.Handle, []byte("indeterminate")) {
		t.Fatalf("interrupted=%+v", completed)
	}
}

func TestFirstDiagnosticsRequestWaitsForVersionedPublication(t *testing.T) {
	manager, _ := newTestManager(t, "utf-16")
	result := execute(t, manager, Request{Language: "go", Action: "diagnostics", Path: "a.go"})
	if !bytes.Contains(result.Data, []byte("example")) {
		t.Fatalf("diagnostics=%s", result.Data)
	}
}
func TestCodeActionSuccessAndGenerationInvalidation(t *testing.T) {
	manager, root := newTestManager(t, "utf-16")
	result := execute(t, manager, Request{Language: "go", Action: "code_actions", Path: "a.go"})
	var actions []ActionSummary
	_ = json.Unmarshal(result.Data, &actions)
	applied := execute(t, manager, Request{Language: "go", Action: "apply_code_action", ActionID: actions[0].ID})
	if applied.Mutation == nil || applied.Code != "applied" {
		t.Fatalf("applied=%+v", applied)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(data) != "new value\n" {
		t.Fatalf("edit=%s", data)
	}
	result = execute(t, manager, Request{Language: "go", Action: "code_actions", Path: "b.go"})
	_ = json.Unmarshal(result.Data, &actions)
	execute(t, manager, Request{Language: "go", Action: "restart", Generation: result.Generation})
	expired := manager.Execute(t.Context(), "expired-action", Request{Language: "go", Action: "apply_code_action", ActionID: actions[0].ID})
	if expired.Code != "expired" {
		t.Fatalf("action generation=%+v", expired)
	}
}
func TestWorkspaceEditPreflightRejectsForeignURIAndUnknownTargets(t *testing.T) {
	manager, root := newTestManager(t, "utf-16")
	snapshots := map[string]mutation.Snapshot{}
	path := filepath.Join(root, "a.go")
	snapshot, err := manager.mutation.Snapshot(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	snapshots[path] = snapshot
	for _, uri := range []string{"file://remote/path.go", "https://example.com/a.go", fileURI(filepath.Join(t.TempDir(), "escape.go")), fileURI(filepath.Join(root, "unobserved.go"))} {
		_, err := manager.prepare(workspaceEdit{Changes: map[string][]textEdit{uri: {{Range: Range{}, NewText: "bypass"}}}}, snapshots, nil, "utf-16")
		if err == nil {
			t.Fatalf("accepted unsafe URI=%s", uri)
		}
	}
	result := Result{Version: 1, Code: "indeterminate", Mutation: &mutation.Result{Version: 1, Code: mutation.Indeterminate, Targets: []mutation.TargetResult{{Path: "a.go", Code: mutation.Applied}, {Path: "b.go", Code: mutation.Indeterminate}}}}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Result
	if err = json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if len(roundtrip.Mutation.Targets) != 2 || roundtrip.Mutation.Targets[0].Code != mutation.Applied || roundtrip.Mutation.Targets[1].Code != mutation.Indeterminate {
		t.Fatal("partial/indeterminate detail was lost")
	}
}

func TestRecoveredReadyRenameUsesReceiptBeforeServerStartup(t *testing.T) {
	manager, _ := newTestManager(t, "utf-16")
	result := manager.mutation.Execute(t.Context(), "recovered", mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: "a.go", Expected: mutation.RevisionOf([]byte("old value\n")), Content: []byte("already applied")}}})
	if result.Code != mutation.Applied {
		t.Fatal(result)
	}
	recovered := manager.Execute(t.Context(), "recovered", Request{Language: "go", Action: "rename", Path: "a.go", NewName: "new"})
	if recovered.Code != "applied" || recovered.Mutation == nil || len(manager.servers) != 0 {
		t.Fatalf("recovered=%+v", recovered)
	}
}
