//go:build cgo && (linux || darwin)

package ast

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func fixture(t *testing.T) (Executor, *mutation.Service, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	files, err := mutation.New(mutation.Config{Root: t.TempDir(), StateDir: state, Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return Executor{Files: files}, files, state
}
func ptr[T any](v T) *T { return &v }
func query(language string) string {
	switch language {
	case "python":
		return `((call function: (identifier) @callee arguments: (argument_list) @args) @match (#eq? @callee "old"))`
	case "go":
		return `((call_expression function: (identifier) @callee arguments: (argument_list) @args) @match (#eq? @callee "old"))`
	default:
		return `((call_expression function: (identifier) @callee arguments: (arguments) @args) @match (#eq? @callee "old"))`
	}
}
func TestRealParsersPreviewAndApplyAcrossLanguages(t *testing.T) {
	for language, source := range map[string]string{"go": "package p\nfunc f() { old(1) }\n", "javascript": "old(1);\n", "python": "old(1)\n"} {
		t.Run(language, func(t *testing.T) {
			executor, files, _ := fixture(t)
			path := filepath.Join(files.Root(), "source")
			os.WriteFile(path, []byte(source), 0600)
			request := Request{Version: Version, Language: language, Query: query(language), Targets: []Target{{Path: "source"}}, Replacement: ptr("updated${args}")}
			preview := executor.Execute(t.Context(), "preview", request)
			if preview.Code != "ok" || preview.Count != 1 || len(preview.Files) != 1 || preview.Files[0].Matches[0].Replacement != "updated(1)" {
				t.Fatal(preview)
			}
			before, _ := os.ReadFile(path)
			if string(before) != source {
				t.Fatal("preview wrote file")
			}
			request.Targets[0].Expected = &preview.Files[0].Revision
			request.ExpectedMatches = ptr(1)
			request.Apply = true
			applied := executor.Execute(t.Context(), "apply", request)
			if applied.Code != "applied" || applied.Count != 1 {
				t.Fatal(applied)
			}
			after, _ := os.ReadFile(path)
			if string(after) != strings.ReplaceAll(source, "old(1)", "updated(1)") {
				t.Fatal(string(after))
			}
			// Replaying the original plan returns outcome evidence, not a new AST pass.
			if again := executor.Execute(t.Context(), "apply", request); again.Code != "applied" {
				t.Fatal(again)
			}
			if stale := executor.Execute(t.Context(), "another", request); stale.Code != "stale" {
				t.Fatal(stale)
			}
		})
	}
}
func TestRejectMalformedAmbiguousUnsupportedAndNoMatch(t *testing.T) {
	cases := []struct{ name, source, query, replacement, want string }{
		{"malformed_source", "function f( {", "(identifier) @match", "name", "malformed_source"},
		{"missing_node", "let a = ;", "(identifier) @match", "name", "malformed_source"},
		{"query", "old(1);", "(made_up_node) @match", "new", "invalid_query"},
		{"no_capture", "old(1);", "(identifier)", "new", "match_capture_required"},
		{"overlap", "old(old(1));", "(call_expression) @match", "updated(1)", "overlapping_matches"},
		{"bad_replacement", "old(1);", query("javascript"), "(", "malformed_replacement"},
		{"unknown_capture", "old(1);", query("javascript"), "${missing}", "unknown_capture"},
		{"unknown_predicate", "old(1);", `((call_expression) @match (#unknown? @match))`, "updated(1)", "unsupported_predicate"},
		{"no_match", "other(1);", query("javascript"), "updated(1)", "no_match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor, files, _ := fixture(t)
			os.WriteFile(filepath.Join(files.Root(), "source"), []byte(tc.source), 0600)
			result := executor.Execute(t.Context(), "preview", Request{Version: Version, Language: "javascript", Query: tc.query, Targets: []Target{{Path: "source"}}, Replacement: &tc.replacement})
			if result.Code != tc.want {
				t.Fatal(result)
			}
		})
	}
	executor, _, _ := fixture(t)
	if got := executor.Execute(t.Context(), "unsupported", Request{Version: Version, Language: "rust", Query: "x", Targets: []Target{{Path: "source"}}}); got.Code != "unsupported_language" {
		t.Fatal(got)
	}
}
func TestAllTargetsPreflightBeforeWrite(t *testing.T) {
	executor, files, _ := fixture(t)
	os.WriteFile(filepath.Join(files.Root(), "a"), []byte("old(1);"), 0600)
	os.WriteFile(filepath.Join(files.Root(), "b"), []byte("broken( {"), 0600)
	req := Request{Version: Version, Language: "javascript", Query: query("javascript"), Replacement: ptr("updated${args}"), Apply: true, ExpectedMatches: ptr(2), Targets: []Target{{Path: "a", Expected: ptr(mutation.RevisionOf([]byte("old(1);")))}, {Path: "b", Expected: ptr(mutation.RevisionOf([]byte("broken( {")))}}}
	result := executor.Execute(t.Context(), "invalid-batch", req)
	if result.Code != "malformed_source" {
		t.Fatal(result)
	}
	data, _ := os.ReadFile(filepath.Join(files.Root(), "a"))
	if string(data) != "old(1);" {
		t.Fatal("partial validation write", string(data))
	}
	req.Targets = req.Targets[:1]
	req.ExpectedMatches = ptr(9)
	if result := executor.Execute(t.Context(), "count-conflict", req); result.Code != "match_count_conflict" {
		t.Fatal(result)
	}
	req.Targets = append(req.Targets, Target{Path: "./a", Expected: req.Targets[0].Expected})
	if result := executor.Execute(t.Context(), "alias", req); result.Code != "duplicate_target" {
		t.Fatal(result)
	}
}
func TestBoundedPreviewAndCancellation(t *testing.T) {
	executor, files, _ := fixture(t)
	source := strings.Repeat("old(1);\n", 100)
	os.WriteFile(filepath.Join(files.Root(), "a"), []byte(source), 0600)
	req := Request{Version: Version, Language: "javascript", Query: query("javascript"), Replacement: ptr("updated${args}"), Targets: []Target{{Path: "a"}}, MaxBytes: 16}
	result := executor.Execute(t.Context(), "preview", req)
	if result.Code != "ok" || result.Count != 100 || !result.Truncated || len(result.Files[0].Matches) > 2 {
		t.Fatal(result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := executor.Execute(ctx, "canceled", req); result.Code != "canceled" {
		t.Fatal(result)
	}
}
func TestIndeterminateRecoveryBeforeASTRebuild(t *testing.T) {
	executor, files, state := fixture(t)
	req := Request{Version: Version, Language: "javascript", Query: query("javascript"), Replacement: ptr("updated${args}"), Targets: []Target{{Path: "a", Expected: ptr(mutation.RevisionOf([]byte("old(1);")))}}, Apply: true, ExpectedMatches: ptr(1), MaxBytes: 16 << 10}
	plan, _ := json.Marshal(req)
	digest := sha256.Sum256(plan)
	id := sha256.Sum256([]byte("lost"))
	receipt := []byte("{\"version\":1,\"digest\":\"" + hex.EncodeToString(digest[:]) + "\"}")
	os.WriteFile(filepath.Join(state, hex.EncodeToString(id[:])+".receipt"), receipt, 0600)
	os.WriteFile(filepath.Join(files.Root(), "a"), []byte("updated(1);"), 0600)
	if got := executor.Execute(t.Context(), "lost", req); got.Code != "indeterminate" {
		t.Fatal(got)
	}
	data, _ := os.ReadFile(filepath.Join(files.Root(), "a"))
	if string(data) != "updated(1);" {
		t.Fatal("recovery changed file")
	}
}

type partialService struct{ *mutation.Service }

func (s partialService) ExecutePrepared(ctx context.Context, _ string, _ []byte, prepare func(context.Context) (mutation.Request, error)) mutation.Result {
	request, err := prepare(ctx)
	if err != nil {
		return mutation.Result{Version: 1, Code: mutation.Invalid}
	}
	// Deterministic late failure injected at the common service boundary. The
	// mutation package separately kills real processes around actual replacements.
	first := s.Service.Apply(ctx, mutation.Request{Version: 1, Changes: request.Changes[:1]})
	return mutation.Result{Version: 1, Code: mutation.Failed, Targets: []mutation.TargetResult{first.Targets[0], {Path: request.Changes[1].Path, Code: mutation.Failed}}}
}
func TestPartialOutcomeIsNeverSuccessOrRollback(t *testing.T) {
	_, files, _ := fixture(t)
	for _, path := range []string{"a", "b"} {
		os.WriteFile(filepath.Join(files.Root(), path), []byte("old(1);"), 0600)
	}
	executor := Executor{Files: partialService{files}}
	revision := mutation.RevisionOf([]byte("old(1);"))
	req := Request{Version: 1, Language: "javascript", Query: query("javascript"), Replacement: ptr("updated${args}"), Targets: []Target{{Path: "a", Expected: &revision}, {Path: "b", Expected: &revision}}, Apply: true, ExpectedMatches: ptr(2)}
	got := executor.Execute(t.Context(), "partial", req)
	if got.Code != "failed" || got.Mutation.Targets[0].Code != mutation.Applied || got.Mutation.Targets[1].Code != mutation.Failed {
		t.Fatal(got)
	}
	a, _ := os.ReadFile(filepath.Join(files.Root(), "a"))
	b, _ := os.ReadFile(filepath.Join(files.Root(), "b"))
	if string(a) != "updated(1);" || string(b) != "old(1);" {
		t.Fatal(string(a), string(b))
	}
}
func TestASTChild(t *testing.T) {
	if os.Getenv("UA_AST_TEST") == "" {
		return
	}
	files, err := mutation.New(mutation.Config{Root: os.Getenv("UA_ROOT"), StateDir: os.Getenv("UA_STATE"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		os.Exit(12)
	}
	for {
		if _, err := os.Stat(filepath.Join(os.Getenv("UA_STATE"), "go")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	req := Request{Version: 1, Language: "javascript", Query: query("javascript"), Replacement: ptr(os.Getenv("UA_VALUE") + "${args}"), Targets: []Target{{Path: "a", Expected: ptr(mutation.RevisionOf([]byte("old(1);")))}}, Apply: true, ExpectedMatches: ptr(1)}
	got := (Executor{Files: files}).Execute(context.Background(), os.Getenv("UA_VALUE"), req)
	if got.Code == "applied" {
		os.Exit(0)
	}
	if got.Code == "stale" {
		os.Exit(3)
	}
	os.Exit(13)
}
func TestCompetingStructuralEditsInRealProcesses(t *testing.T) {
	_, files, state := fixture(t)
	os.WriteFile(filepath.Join(files.Root(), "a"), []byte("old(1);"), 0600)
	child := func(name string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestASTChild$")
		cmd.Env = append(os.Environ(), "UA_AST_TEST=1", "UA_ROOT="+files.Root(), "UA_STATE="+state, "UA_VALUE="+name)
		return cmd
	}
	a, b := child("one"), child("two")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(state, "go"), nil, 0600)
	success, stale := 0, 0
	for _, err := range []error{a.Wait(), b.Wait()} {
		if err == nil {
			success++
			continue
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			stale++
			continue
		}
		t.Fatal(err)
	}
	if success != 1 || stale != 1 {
		t.Fatal(success, stale)
	}
}
func TestSerializableOperationPermissionAndCancel(t *testing.T) {
	root := t.TempDir()
	policy := permission.DenyAll()
	ctx := permission.WithPolicy(t.Context(), policy)
	files, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Handler itself enforces file policy even without a Manager tool check.
	handler, _ := NewHandler(ctx, Executor{Files: files}, "session")
	spec, err := Spec(Request{Version: 1, Language: "javascript", Query: query("javascript"), Targets: []Target{{Path: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	original := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	encoded, _ := json.Marshal(original)
	var current operation.Operation
	if json.Unmarshal(encoded, &current) != nil {
		t.Fatal("roundtrip")
	}
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-handler.RemoteJobUpdates():
		if got.Status != operation.StatusFailed || got.Denial == nil {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked")
	}
	current.ID = "canceled"
	current.Status = operation.StatusCanceling
	if err = handler.AddRemoteJob(current); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-handler.RemoteJobUpdates():
		if got.Status != operation.StatusCanceled {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked")
	}
}
func BenchmarkStructuralPreview(b *testing.B) {
	source := []byte(strings.Repeat("old(1);\n", 100))
	replacement := "updated${args}"
	b.ResetTimer()
	for b.Loop() {
		result, err := analyze(b.Context(), "javascript", query("javascript"), source, &replacement)
		if err != nil || len(result.matches) != 100 {
			b.Fatal(err)
		}
	}
}
