//go:build linux || darwin

package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func executor(t *testing.T) (Executor, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	service, err := mutation.New(mutation.Config{Root: t.TempDir(), StateDir: state, Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return Executor{Files: service}, state
}
func execute(e Executor, t *testing.T, r Request) Result {
	t.Helper()
	r.Version = Version
	return e.Execute(t.Context(), t.Name()+"/"+r.Action, r)
}
func TestReadRangeRevisionBinaryAndLarge(t *testing.T) {
	e, _ := executor(t)
	text := "zero\n日本語\nlast\n"
	os.WriteFile(filepath.Join(e.Files.Root(), "text"), []byte(text), 0600)
	result := execute(e, t, Request{Action: "read", Path: "text", StartLine: 2, Lines: 1, MaxBytes: 6})
	if result.Code != "ok" || result.Content != "日本" || !result.Truncated || *result.Revision != mutation.RevisionOf([]byte(text)) {
		t.Fatal(result)
	}
	os.WriteFile(filepath.Join(e.Files.Root(), "binary"), []byte{0, 255}, 0600)
	if got := execute(e, t, Request{Action: "read", Path: "binary"}); got.Code != "binary" || !got.Binary {
		t.Fatal(got)
	}
	file, err := os.Create(filepath.Join(e.Files.Root(), "huge"))
	if err != nil {
		t.Fatal(err)
	}
	file.Truncate(mutation.MaxFileBytes + 1)
	file.Close()
	if got := execute(e, t, Request{Action: "read", Path: "huge"}); got.Code != "read_failed" {
		t.Fatal(got)
	}
}
func TestWriteEditPreconditionsAndDurableResult(t *testing.T) {
	e, _ := executor(t)
	absent := mutation.Revision{}
	write := Request{Version: Version, Action: "write", Path: "a", Expected: &absent, Content: "old old"}
	if got := e.Execute(t.Context(), "write", write); got.Code != "applied" {
		t.Fatal(got)
	}
	revision := mutation.RevisionOf([]byte("old old"))
	edit := Request{Version: Version, Action: "edit", Path: "a", Expected: &revision, OldText: "old", NewText: "new"}
	if got := e.Execute(t.Context(), "ambiguous", edit); got.Code != "ambiguous" {
		t.Fatal(got)
	}
	edit.ReplaceAll = true
	if got := e.Execute(t.Context(), "edit", edit); got.Code != "applied" {
		t.Fatal(got)
	}
	// Reconstructing an already-executed edit from newer content would incorrectly
	// return stale. Durable outcome evidence takes precedence without replay.
	os.WriteFile(filepath.Join(e.Files.Root(), "a"), []byte("external later"), 0600)
	if got := e.Execute(t.Context(), "edit", edit); got.Code != "applied" {
		t.Fatal(got)
	}
	got, _ := e.Files.Snapshot(t.Context(), "a")
	if string(got.Data) != "external later" {
		t.Fatal("edit replayed")
	}
	if got := e.Execute(t.Context(), "new-stale", edit); got.Code != "stale" {
		t.Fatal(got)
	}
	if got := e.Execute(t.Context(), "new-write", write); got.Code != "stale" {
		t.Fatal(got)
	}
}
func TestNativeIndeterminateReceipt(t *testing.T) {
	e, state := executor(t)
	revision := mutation.RevisionOf([]byte("old"))
	r := Request{Version: Version, Action: "edit", Path: "a", Expected: &revision, OldText: "old", NewText: "new", MaxBytes: 16 << 10, Limit: 200}
	wire, _ := json.Marshal(r)
	digest := sha256.Sum256(wire)
	id := sha256.Sum256([]byte("lost"))
	receipt := []byte("{\"version\":1,\"digest\":\"" + hex.EncodeToString(digest[:]) + "\"}")
	os.WriteFile(filepath.Join(state, hex.EncodeToString(id[:])+".receipt"), receipt, 0600)
	os.WriteFile(filepath.Join(e.Files.Root(), "a"), []byte("new"), 0600)
	if got := e.Execute(t.Context(), "lost", r); got.Code != "indeterminate" {
		t.Fatal(got)
	}
}
func TestSearchScopeOrderingLimitsAndCancellation(t *testing.T) {
	e, _ := executor(t)
	for _, p := range []string{"b.go", "a.go", "sub/c.go", "sub/z.txt"} {
		path := filepath.Join(e.Files.Root(), p)
		os.MkdirAll(filepath.Dir(path), 0700)
		os.WriteFile(path, []byte("needle\nother\nneedle\n"), 0600)
	}
	os.Symlink("a.go", filepath.Join(e.Files.Root(), "alias.go"))
	r := Request{Version: Version, Action: "grep", Path: ".", Pattern: "needle", Glob: "**/*.go", Limit: 3}
	one := e.Execute(t.Context(), "one", r)
	two := e.Execute(t.Context(), "two", r)
	if !reflect.DeepEqual(one, two) || !one.Truncated || len(one.Matches) != 3 || one.Matches[0].Path != "a.go" || one.Matches[2].Path != "b.go" {
		t.Fatal(one, two)
	}
	r.Action = "glob"
	r.Limit = 100
	glob := e.Execute(t.Context(), "glob", r)
	if len(glob.Matches) != 3 || glob.Matches[2].Path != filepath.Join("sub", "c.go") {
		t.Fatal(glob)
	}
	r.Action = "grep"
	r.MaxBytes = 8
	if out := e.Execute(t.Context(), "bound", r); !out.Truncated {
		t.Fatal(out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if out := e.Execute(ctx, "canceled", r); out.Code != "canceled" {
		t.Fatal(out)
	}
}

type denial struct{}

func (denial) Error() string          { return "permission denied" }
func (denial) PermissionDenied() bool { return true }
func TestDeniedReadSearchAndWrite(t *testing.T) {
	root := t.TempDir()
	files, err := mutation.New(mutation.Config{Root: root, StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(context.Context, string, bool) error { return denial{} }})
	if err != nil {
		t.Fatal(err)
	}
	e := Executor{Files: files}
	for _, action := range []string{"read", "write", "grep", "glob"} {
		r := Request{Version: Version, Action: action, Path: "a", Expected: &mutation.Revision{}}
		if action == "grep" || action == "glob" {
			r.Path = "."
		}
		if got := e.Execute(t.Context(), action, r); got.Code != "permission_denied" {
			t.Fatal(action, got)
		}
	}
}
func TestNativeOperationSerializationAndManager(t *testing.T) {
	e, _ := executor(t)
	handler, _ := NewHandler(t.Context(), e, "session")
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	spec, err := Spec(Request{Version: Version, Action: "write", Path: "a", Expected: &mutation.Revision{}, Content: "done"})
	if err != nil {
		t.Fatal(err)
	}
	current := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, MaxOutputLength: spec.MaxOutputLength, State: spec.State, Status: operation.StatusReady}
	wire, _ := json.Marshal(current)
	var restored operation.Operation
	if err = json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	if err = manager.Add(restored); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-manager.Updates():
		if got.Status != operation.StatusCompleted {
			t.Fatal(got)
		}
		state, _ := operation.DecodeRemoteJobState(got)
		var result Result
		if json.Unmarshal(state.Handle, &result) != nil || result.Code != "applied" {
			t.Fatal(state)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked")
	}
}
func TestNativeChild(t *testing.T) {
	if os.Getenv("UA_NATIVE_TEST") == "" {
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
	revision := mutation.RevisionOf([]byte("old"))
	r := Request{Version: Version, Action: "edit", Path: "a", Expected: &revision, OldText: "old", NewText: os.Getenv("UA_VALUE")}
	result := (Executor{Files: files}).Execute(context.Background(), os.Getenv("UA_VALUE"), r)
	if result.Code == "applied" {
		os.Exit(0)
	}
	if result.Code == "stale" {
		os.Exit(3)
	}
	os.Exit(13)
}
func TestNativeTwoRealProcessesEditOneSnapshot(t *testing.T) {
	e, state := executor(t)
	os.WriteFile(filepath.Join(e.Files.Root(), "a"), []byte("old"), 0600)
	child := func(value string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeChild$")
		cmd.Env = append(os.Environ(), "UA_NATIVE_TEST=1", "UA_ROOT="+e.Files.Root(), "UA_STATE="+state, "UA_VALUE="+value)
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
func TestGlobValidation(t *testing.T) {
	for _, test := range []struct {
		pattern, path string
		match         bool
	}{{"**/*.go", "a.go", true}, {"**/*.go", "a/b.go", true}, {"*.go", "a/b.go", false}, {"sub/?[ab].go", "sub/za.go", true}} {
		got, err := MatchGlob(test.pattern, test.path)
		if err != nil || got != test.match {
			t.Fatal(test, got, err)
		}
	}
	if _, err := MatchGlob("[", "a"); err == nil {
		t.Fatal("bad glob accepted")
	}
}
func BenchmarkBoundedRead(b *testing.B) {
	files, err := mutation.New(mutation.Config{Root: b.TempDir(), StateDir: filepath.Join(b.TempDir(), "state"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	os.WriteFile(filepath.Join(files.Root(), "a"), []byte(strings.Repeat("line\n", 1000)), 0600)
	e := Executor{Files: files}
	r := Request{Version: Version, Action: "read", Path: "a", StartLine: 100, Lines: 20}
	b.ResetTimer()
	for b.Loop() {
		if e.Execute(b.Context(), "read", r).Code != "ok" {
			b.Fatal("read failed")
		}
	}
}
