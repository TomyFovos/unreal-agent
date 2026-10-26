package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func newSerializedRestoreFixture(t *testing.T) (*Session, *serializedStore, string) {
	t.Helper()
	directory := t.TempDir()
	raw, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	const id session.ID = "serialized-restore"
	lease, err := raw.AcquireWriter(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	snapshot, err := raw.Create(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"one", "two"} {
		if err = raw.AppendInput(t.Context(), id, input(key, "synthetic history")); err != nil {
			t.Fatal(err)
		}
	}
	s := &Session{ID: id, Generation: "synthetic-owner", store: raw, lock: lease, snapshot: snapshot}
	return s, &serializedStore{s}, filepath.Join(directory, string(id)+".session.jsonl")
}

func assertRestoreMutexReleased(t *testing.T, s *Session) {
	t.Helper()
	if !s.mu.TryLock() {
		t.Fatal("restore left Session writer serialization locked")
	}
	s.mu.Unlock()
}

func TestSerializedHistoryViewSessionBindingAndClose(t *testing.T) {
	s, store, _ := newSerializedRestoreFixture(t)
	if view, err := store.OpenHistoryView(t.Context(), "another-session"); !errors.Is(err, ErrConflict) || view != nil {
		t.Fatalf("cross-session restore view=%v error=%v", view, err)
	}
	assertRestoreMutexReleased(t, s)
	view, err := store.OpenHistoryView(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("history view does not hold its Session writer serialization")
	}
	if _, err = view.Items(t.Context(), "another-session", 0, 1); err == nil {
		t.Fatal("view accepted another Session ID")
	}
	first, err := view.Items(t.Context(), s.ID, 0, 1)
	if err != nil || len(first.Items) != 1 || first.NextAfter != 1 || !first.More {
		t.Fatal("first restore page changed", first, err)
	}
	second, err := view.Items(t.Context(), s.ID, first.NextAfter, 1)
	if err != nil || len(second.Items) != 1 || second.NextAfter != 2 || second.More {
		t.Fatal("second restore page changed", second, err)
	}
	if err = view.Close(); err != nil {
		t.Fatal(err)
	}
	if err = view.Close(); err != nil {
		t.Fatal("idempotent close changed its result", err)
	}
	assertRestoreMutexReleased(t, s)
	if _, err = view.Items(t.Context(), s.ID, 0, 1); !errors.Is(err, localfile.ErrRestoreViewClosed) {
		t.Fatal("closed Host view still readable", err)
	}
	next, err := store.OpenHistoryView(t.Context(), s.ID)
	if err != nil {
		t.Fatal("closed view prevented a new scoped restore", err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSerializedHistoryViewHoldsCanonicalWriteUntilClose(t *testing.T) {
	s, store, _ := newSerializedRestoreFixture(t)
	view, err := store.OpenHistoryView(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started) // Positive start handshake, before the serialized append.
		done <- store.AppendInput(t.Context(), s.ID, input("after-restore", "new canonical boundary"))
	}()
	<-started
	// Establish the actual held mutex in addition to observing an unfinished
	// append. No scheduling delay is treated as proof that a lock is held.
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("restore did not exclude the serialized canonical writer")
	}
	select {
	case err := <-done:
		t.Fatal("canonical append returned while its restore boundary was open", err)
	default:
	}
	page, err := s.store.Items(t.Context(), s.ID, 0, 100)
	if err != nil || len(page.Items) != 2 || page.NextAfter != 2 {
		t.Fatal("raw canonical append crossed the held Session boundary", page, err)
	}
	if err = view.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("writer did not resume after Close", err)
		}
	case <-timeout(t).Done():
		t.Fatal("Close left the serialized canonical append blocked")
	}
	page, err = s.store.Items(t.Context(), s.ID, 0, 100)
	if err != nil || len(page.Items) != 3 || page.NextAfter != 3 {
		t.Fatal("resumed writer failed to persist exactly one new boundary", page, err)
	}
}

func TestSerializedHistoryViewCancelAndInvalidationReleaseOnClose(t *testing.T) {
	for _, mode := range []string{"cancel", "raw_append_invalidation"} {
		t.Run(mode, func(t *testing.T) {
			s, store, _ := newSerializedRestoreFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			view, err := store.OpenHistoryView(ctx, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			want := context.Canceled
			if mode == "cancel" {
				cancel()
			} else {
				// Direct raw Store calls bypass Session serialization by its
				// existing contract. Such a write must invalidate, not mix, pages.
				want = localfile.ErrRestoreViewChanged
				if err = s.store.AppendInput(t.Context(), s.ID, input("outside-view", "raw append invalidates old view")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = view.Items(t.Context(), s.ID, 0, 1); !errors.Is(err, want) {
				t.Fatal("invalid restore view returned a page", err)
			}
			if err = view.Close(); !errors.Is(err, want) {
				t.Fatal("Close lost the cancellation or invalidation failure", err)
			}
			assertRestoreMutexReleased(t, s)
			if err = store.AppendInput(t.Context(), s.ID, input("after-close", "writer remains usable")); err != nil {
				t.Fatal("failed restore left the canonical writer unusable", err)
			}
		})
	}
}

func TestSerializedHistoryViewOpenFailureReleasesSession(t *testing.T) {
	s, store, path := newSerializedRestoreFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(before, []byte("{invalid committed record}\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if view, err := store.OpenHistoryView(t.Context(), s.ID); err == nil || view != nil {
		t.Fatal("committed decode failure produced a Host restore view", err)
	}
	assertRestoreMutexReleased(t, s)
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	view, err := store.OpenHistoryView(t.Context(), s.ID)
	if err != nil {
		t.Fatal("failed construction leaked an active view or writer descriptor", err)
	}
	if err = view.Close(); err != nil {
		t.Fatal(err)
	}
	assertRestoreMutexReleased(t, s)
}

var _ sessionstore.HistoryViewStore = (*serializedStore)(nil)
