//go:build linux || darwin

package mutation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func service(t *testing.T) *Service {
	t.Helper()
	s, err := New(Config{Root: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(path string, old Revision, text string) Request {
	return Request{Version: Version, Changes: []Change{{Path: path, Expected: old, Content: []byte(text)}}}
}
func TestConditionalReplacementAndPreflight(t *testing.T) {
	s := service(t)
	r := s.Apply(t.Context(), request("a", Revision{}, "abc"))
	if r.Code != Applied {
		t.Fatal(r)
	}
	snap, err := s.Snapshot(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != RevisionOf([]byte("abc")) {
		t.Fatal(snap)
	}
	r = s.Apply(t.Context(), request("a", snap.Revision, "xyz"))
	if r.Code != Applied {
		t.Fatal(r)
	}
	r = s.Apply(t.Context(), request("a", snap.Revision, "old"))
	if r.Code != Stale {
		t.Fatal(r)
	}
	r = s.Apply(t.Context(), Request{Version: Version, Changes: []Change{{Path: "a", Expected: RevisionOf([]byte("xyz")), Content: []byte("bad")}, {Path: "b", Expected: RevisionOf([]byte("missing")), Content: []byte("bad")}}})
	if r.Code != Stale {
		t.Fatal(r)
	}
	got, _ := s.Snapshot(t.Context(), "a")
	if string(got.Data) != "xyz" {
		t.Fatal(got)
	}
	for _, path := range []string{"../outside", s.root, strings.Repeat("a", 4097)} {
		if got := s.Apply(t.Context(), request(path, Revision{}, "x")); got.Code != Invalid {
			t.Fatal(got)
		}
	}
	// lexical aliases share one target identity; duplicate batch targets reject.
	r = s.Apply(t.Context(), Request{Version: Version, Changes: []Change{{Path: "a", Expected: RevisionOf([]byte("xyz"))}, {Path: "./a", Expected: RevisionOf([]byte("xyz"))}}})
	if r.Code != Invalid {
		t.Fatal(r)
	}
}
func TestRejectLinksAndNonregularTargets(t *testing.T) {
	s := service(t)
	os.WriteFile(filepath.Join(s.root, "real"), []byte("x"), 0600)
	if err := os.Symlink("real", filepath.Join(s.root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(t.Context(), "link"); err == nil {
		t.Fatal("read symlink allowed")
	}
	if r := s.Apply(t.Context(), request("link", RevisionOf([]byte("x")), "y")); r.Code != Invalid {
		t.Fatal(r)
	}
	os.Mkdir(filepath.Join(s.root, "dir"), 0700)
	os.Symlink("dir", filepath.Join(s.root, "alias"))
	if r := s.Apply(t.Context(), request("alias/a", Revision{}, "y")); r.Code != Invalid {
		t.Fatal(r)
	}
	os.Link(filepath.Join(s.root, "real"), filepath.Join(s.root, "hard"))
	if r := s.Apply(t.Context(), request("hard", RevisionOf([]byte("x")), "y")); r.Code != Invalid {
		t.Fatal(r)
	}
	if r := s.Apply(t.Context(), request("dir", Revision{}, "y")); r.Code != Invalid {
		t.Fatal(r)
	}
}
func TestPermissionRecheckAndCancellation(t *testing.T) {
	s := service(t)
	count := 0
	s.authorize = func(_ context.Context, _ string, write bool) error {
		if write {
			count++
			return errors.New("denied")
		}
		return nil
	}
	if r := s.Apply(t.Context(), request("a", Revision{}, "x")); r.Code != Denied || count != 1 {
		t.Fatal(r, count)
	}
	if _, err := os.Stat(filepath.Join(s.root, "a")); !os.IsNotExist(err) {
		t.Fatal("denied write applied")
	}
	s.authorize = func(context.Context, string, bool) error { return nil }
	lock, err := s.lock(t.Context(), filepath.Join(s.root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if r := s.Apply(ctx, request("a", Revision{}, "x")); r.Code != Canceled {
		t.Fatal(r)
	}
}
func TestPartialFailureAndReceiptReplay(t *testing.T) {
	s := service(t)
	req := Request{Version: Version, Changes: []Change{{Path: "a", Content: []byte("a")}, {Path: "b", Content: []byte("b")}}}
	s.hook = func(stage string, index int) error {
		if stage == "before_replace" && index == 1 {
			return errors.New("disk failure")
		}
		return nil
	}
	r := s.Execute(t.Context(), "session/op", req)
	if r.Code != Failed || r.Targets[0].Code != Applied || r.Targets[1].Code != Failed {
		t.Fatal(r)
	}
	s.hook = nil
	replay := s.Execute(t.Context(), "session/op", req)
	if replay.Code != Failed || replay.Targets[0].Code != Applied {
		t.Fatal(replay)
	}
	if _, err := os.Stat(filepath.Join(s.root, "b")); !os.IsNotExist(err) {
		t.Fatal("replayed partial operation")
	}
	if r := s.Execute(t.Context(), "session/op", request("other", Revision{}, "x")); r.Code != Indeterminate {
		t.Fatal("id payload conflict", r)
	}
}
func TestNoncooperatingWriterIsOutsideGuarantee(t *testing.T) {
	s := service(t)
	s.hook = func(stage string, _ int) error {
		if stage == "before_replace" {
			return os.WriteFile(filepath.Join(s.root, "a"), []byte("external"), 0600)
		}
		return nil
	}
	r := s.Apply(t.Context(), request("a", Revision{}, "cooperating"))
	if r.Code != Applied {
		t.Fatal(r)
	}
	// This deliberately documents that direct writers do not honor our lock.
	got, _ := os.ReadFile(filepath.Join(s.root, "a"))
	if string(got) != "cooperating" {
		t.Fatal(string(got))
	}
}
func TestMutationChild(t *testing.T) {
	mode := os.Getenv("UA_MUTATION_TEST")
	if mode == "" {
		return
	}
	s, err := New(Config{Root: os.Getenv("UA_ROOT"), StateDir: os.Getenv("UA_STATE"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		os.Exit(12)
	}
	req := request("a", Revision{}, os.Getenv("UA_CONTENT"))
	if mode == "race" {
		for {
			if _, err := os.Stat(filepath.Join(s.state, "go")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		r := s.Execute(context.Background(), os.Getenv("UA_ID"), req)
		if r.Code == Applied {
			os.Exit(0)
		}
		if r.Code == Stale {
			os.Exit(3)
		}
		os.Exit(13)
	}
	s.hook = func(stage string, _ int) error {
		if stage == mode {
			os.Exit(23)
		}
		return nil
	}
	s.Execute(context.Background(), "crash", req)
	os.Exit(14)
}
func child(t *testing.T, s *Service, mode, id, content string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMutationChild$")
	cmd.Env = append(os.Environ(), "UA_MUTATION_TEST="+mode, "UA_ROOT="+s.root, "UA_STATE="+s.state, "UA_ID="+id, "UA_CONTENT="+content)
	return cmd
}
func TestSeparateProcessesCompete(t *testing.T) {
	s := service(t)
	a := child(t, s, "race", "a", "a")
	b := child(t, s, "race", "b", "b")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.state, "go"), nil, 0600)
	ea, eb := a.Wait(), b.Wait()
	success := 0
	stale := 0
	for _, err := range []error{ea, eb} {
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
func TestCrashWindowsRecoverIndeterminate(t *testing.T) {
	for _, stage := range []string{"after_intent", "before_replace", "after_replace", "before_receipt"} {
		t.Run(stage, func(t *testing.T) {
			s := service(t)
			cmd := child(t, s, stage, "crash", "value")
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(s.root, "a"))
			r := s.Execute(t.Context(), "crash", request("a", Revision{}, "value"))
			if r.Code != Indeterminate {
				t.Fatal(r)
			}
			after, _ := os.ReadFile(filepath.Join(s.root, "a"))
			if string(before) != string(after) {
				t.Fatal("unknown operation replayed")
			}
			if (stage == "after_replace" || stage == "before_receipt") && string(after) != "value" {
				t.Fatal(string(after))
			}
			if (stage == "after_intent" || stage == "before_replace") && len(after) != 0 {
				t.Fatal("unexpected write")
			}
		})
	}
}
func TestSerializableOperationAndRecovery(t *testing.T) {
	s := service(t)
	req := request("a", Revision{}, "value")
	spec, err := Spec(req)
	if err != nil {
		t.Fatal(err)
	}
	original := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
	wire, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored operation.Operation
	if err = json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	handler, _ := NewHandler(t.Context(), s, "session")
	manager := operation.NewLocalOperationManager(t.Context(), handler)
	if err = manager.Add(restored); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-manager.Updates():
		if got.Status != operation.StatusCompleted {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("operation blocked")
	}
	// Even the original ready checkpoint can safely be redispatched after restart.
	recovered, _ := NewHandler(t.Context(), s, "session")
	if err = recovered.AddRemoteJob(original); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-recovered.RemoteJobUpdates():
		if got.Status != operation.StatusCompleted {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("recovery blocked")
	}
}
func BenchmarkMutationPrecondition(b *testing.B) {
	s, err := New(Config{Root: b.TempDir(), StateDir: filepath.Join(b.TempDir(), "state"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	s.Apply(b.Context(), request("a", Revision{}, strings.Repeat("a", 8192)))
	req := request("a", RevisionOf([]byte("outdated")), "new")
	b.ResetTimer()
	for b.Loop() {
		if s.Apply(b.Context(), req).Code != Stale {
			b.Fatal("wrong result")
		}
	}
}
