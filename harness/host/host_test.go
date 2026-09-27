package host

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type modelFunc func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (f modelFunc) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	return f(ctx, r, o)
}
func factory(model llm.Adapter) Factory {
	return func(ctx context.Context, _ session.ID) (Runtime, error) {
		return Runtime{Builder: contextbuilder.NewBuilder(), LLM: model, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}
}
func echo(ctx context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "done"}}}}, nil
}
func newTestHost(t *testing.T, dir string) *Host {
	t.Helper()
	h, err := New(t.Context(), Config{Directory: dir, Build: factory(modelFunc(echo))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func input(id, text string) inbox.Input {
	data, _ := json.Marshal(text)
	return inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: data}
}
func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, c := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(c)
	return ctx
}
func stop(t *testing.T, s *Session) {
	t.Helper()
	ctx := timeout(t)
	if _, err := s.Stop(ctx, s.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptConcurrentDuplicateConflictAndResume(t *testing.T) {
	h := newTestHost(t, t.TempDir())
	s, err := h.Create(t.Context(), Options{ID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	const count = 20
	var wg sync.WaitGroup
	receipts := make(chan Receipt, count)
	errs := make(chan error, count)
	for range count {
		wg.Go(func() { r, e := s.Submit(timeout(t), s.Generation, input("one", "hello")); receipts <- r; errs <- e })
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var want Receipt
	for r := range receipts {
		if want.ID == "" {
			want = r
		}
		if r != want {
			t.Fatalf("receipts differ: %+v %+v", want, r)
		}
	}
	if _, e := s.Submit(t.Context(), s.Generation, input("one", "different")); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflict: %v", e)
	}
	if _, e := s.Submit(t.Context(), "old", input("two", "hello")); !errors.Is(e, ErrStaleGeneration) {
		t.Fatalf("generation: %v", e)
	}
	stop(t, s)
	resumed, err := h.Resume(t.Context(), Options{ID: s.ID})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := resumed.Submit(timeout(t), resumed.Generation, input("one", "hello"))
	if err != nil || receipt != want {
		t.Fatalf("resume receipt: %+v %v", receipt, err)
	}
	if _, err = resumed.Submit(timeout(t), resumed.Generation, input("two", "again")); err != nil {
		t.Fatal(err)
	}
	stop(t, resumed)
	v, err := resumed.Inspect(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[inbox.ID]int{}
	for _, item := range v.History.Items {
		if item.Kind == sessionstore.ItemInput {
			seen[item.Data.(inbox.Input).ID]++
		}
	}
	if seen["one"] != 1 || seen["two"] != 1 {
		t.Fatalf("history duplicates: %v", seen)
	}
}

func TestSlowSubscriberGapAndSnapshotIsolation(t *testing.T) {
	h := newTestHost(t, t.TempDir())
	s, e := h.Create(t.Context(), Options{ID: "slow"})
	if e != nil {
		t.Fatal(e)
	}
	sub, e := s.Subscribe(0, 4096, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Cancel()
	for i := range 8 {
		if _, e = s.Submit(timeout(t), s.Generation, input(fmt.Sprintf("i-%d", i), "hello")); e != nil {
			t.Fatal(e)
		}
	}
	event := <-sub.Events
	if event.Kind != "gap" {
		t.Fatalf("expected bounded gap, got %+v", event)
	}
	if _, open := <-sub.Events; open {
		t.Fatal("gapped subscription should close")
	}
	fresh, e := s.Subscribe(0, 4096, 64)
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Cancel()
	if len(fresh.Initial.History.Items) < 9 {
		t.Fatalf("resync lost committed inputs")
	}
	first := fresh.Initial.History.Items[0].Data.(sessionstore.HostRecord)
	first.Configuration[0] = '!'
	// Returned records cannot mutate the owner's cache.
	if _, e = s.Inspect(0, 4096); e != nil {
		t.Fatal(e)
	}
	stop(t, s)
}

func TestOwnershipSafeCreateAndConfiguration(t *testing.T) {
	dir := t.TempDir()
	a := newTestHost(t, dir)
	b := newTestHost(t, dir)
	opts := Options{ID: "owned", Configuration: jsontext.Value(`{"model":"fixed"}`)}
	s, e := a.Create(t.Context(), opts)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.Resume(t.Context(), opts); !errors.Is(e, localfile.ErrWriterOwned) {
		t.Fatalf("two owners: %v", e)
	}
	stop(t, s)
	if _, e = b.Create(t.Context(), opts); !errors.Is(e, os.ErrExist) {
		t.Fatalf("create replaced: %v", e)
	}
	bad := opts
	bad.Configuration = jsontext.Value(`{"model":"other"}`)
	if _, e = b.Resume(t.Context(), bad); e == nil {
		t.Fatal("changed config accepted")
	}
	next, e := b.Resume(t.Context(), opts)
	if e != nil {
		t.Fatal(e)
	}
	if next.Generation == s.Generation {
		t.Fatal("generation reused")
	}
	stop(t, next)
}

func TestPersistenceFailureDoesNotAcknowledge(t *testing.T) {
	dir := t.TempDir()
	h := newTestHost(t, dir)
	s, e := h.Create(t.Context(), Options{ID: "failure"})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(dir, "failure.session.jsonl"), filepath.Join(dir, "saved")); e != nil {
		t.Fatal(e)
	}
	if r, e := s.Submit(timeout(t), s.Generation, input("one", "hello")); e == nil || r.Sequence != 0 {
		t.Fatalf("false durable ACK %+v %v", r, e)
	}
}

func TestPendingHardStopDoesNotDispatchRecoveredWork(t *testing.T) {
	dir := t.TempDir()
	raw, e := localfile.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = raw.Create(t.Context(), "stopped"); e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.StopHard, Reason: "crashed before completion"})
	if e = raw.AppendInput(t.Context(), "stopped", inbox.Input{ID: "stop", Kind: inbox.InputControl, Payload: data}); e != nil {
		t.Fatal(e)
	}
	h := newTestHost(t, dir)
	s, e := h.Resume(t.Context(), Options{ID: "stopped"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Wait(timeout(t)); e != nil {
		t.Fatal(e)
	}
	again, e := h.Resume(t.Context(), Options{ID: "stopped"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = again.Submit(timeout(t), again.Generation, input("after", "new")); e != nil {
		t.Fatal(e)
	}
	stop(t, again)
}

func TestSnapshotSubscribeBarrier(t *testing.T) {
	h := newTestHost(t, t.TempDir())
	s, e := h.Create(t.Context(), Options{ID: "barrier"})
	if e != nil {
		t.Fatal(e)
	}
	for i := range 30 {
		sub, e := s.Subscribe(0, 4096, 64)
		if e != nil {
			t.Fatal(e)
		}
		receipt, e := s.Submit(timeout(t), s.Generation, input(fmt.Sprintf("i-%d", i), "hi"))
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, item := range sub.Initial.History.Items {
			if item.Sequence == receipt.Sequence {
				found = true
			}
		}
		for !found {
			select {
			case event := <-sub.Events:
				if event.Item != nil && event.Item.Sequence == receipt.Sequence {
					found = true
				}
			case <-timeout(t).Done():
				t.Fatal("barrier dropped input")
			}
		}
		sub.Cancel()
	}
	stop(t, s)
}

func TestWriterLockProcessDeath(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriterLockHelper$")
	cmd.Env = append(os.Environ(), "UNREAL_LOCK_HELPER="+dir)
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()
	line, e := bufio.NewReader(out).ReadString('\n')
	if e != nil || line != "locked\n" {
		t.Fatalf("helper: %q %v", line, e)
	}
	raw, e := localfile.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = raw.AcquireWriter("locked"); !errors.Is(e, localfile.ErrWriterOwned) {
		t.Fatalf("cross-process ownership: %v", e)
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	lock, e := raw.AcquireWriter("locked")
	if e != nil {
		t.Fatal(e)
	}
	lock.Close()
}
func TestWriterLockHelper(t *testing.T) {
	dir := os.Getenv("UNREAL_LOCK_HELPER")
	if dir == "" {
		return
	}
	raw, e := localfile.New(dir)
	if e != nil {
		os.Exit(2)
	}
	lock, e := raw.AcquireWriter("locked")
	if e != nil {
		os.Exit(3)
	}
	defer lock.Close()
	fmt.Println("locked")
	select {}
}
