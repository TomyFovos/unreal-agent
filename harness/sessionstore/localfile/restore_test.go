package localfile

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func newRestoreFixture(t *testing.T) (*Store, session.ID, *os.File) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("restore-fixture")
	owner, err := s.AcquireWriter(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	if _, err = s.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, key := range []inbox.ID{"one", "two", "three"} {
		if err = s.AppendInput(t.Context(), id, inbox.Input{ID: key, Kind: inbox.InputExternal, Payload: jsontext.Value(`{"message":"synthetic"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	return s, id, owner
}
func TestRestoreViewLifetimeAndIsolation(t *testing.T) {
	s, id, _ := newRestoreFixture(t)
	v, err := s.OpenRestoreView(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Items(t.Context(), "wrong", 0, 1); err == nil {
		t.Fatal("cross-session access")
	}
	if _, err = v.Items(t.Context(), id, 0, 0); err == nil {
		t.Fatal("invalid page size accepted")
	}
	first, err := v.Items(t.Context(), id, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := first.Items[0].Data.(inbox.Input)
	in.Payload[0] = '!'
	first.Items[0].Data = in
	again, err := v.Items(t.Context(), id, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if again.Items[0].Data.(inbox.Input).Payload[0] != '{' {
		t.Fatal("caller changed immutable view")
	}
	a, err := v.Resume(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Resume(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("resume changed")
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	if err = v.Close(); err != nil {
		t.Fatal(err)
	}
	if v.state != nil || s.restoreView.Load() != nil {
		t.Fatal("closed view retained decoded state")
	}
	if _, err = v.Items(t.Context(), id, 0, 1); !errors.Is(err, ErrRestoreViewClosed) {
		t.Fatal("closed view readable", err)
	}
	next, err := s.OpenRestoreView(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestRestoreViewKeepsSnapshotAfterExternalChange(t *testing.T) {
	for _, mode := range []string{"same-size-restored-mtime", "replace-inode", "torn-append", "replace-lock"} {
		t.Run(mode, func(t *testing.T) {
			s, id, _ := newRestoreFixture(t)
			v, err := s.OpenRestoreView(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			path := s.sessionPath(id)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "same-size-restored-mtime":
				altered := bytes.Replace(original, []byte("synthetic"), []byte("different"), 1)
				if bytes.Equal(altered, original) || len(altered) != len(original) {
					t.Fatal("fixture mutation did not start")
				}
				if err = os.WriteFile(path, altered, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "replace-inode":
				target := filepath.Join(s.directory, "replacement")
				if err = os.WriteFile(target, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(target, path); err != nil {
					t.Fatal(err)
				}
			case "torn-append":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(`{"type":`)
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
			case "replace-lock":
				target := filepath.Join(s.directory, "replacement-lock")
				if err = os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(target, path+".lock"); err != nil {
					t.Fatal(err)
				}
			}
			page, err := v.Items(t.Context(), id, 0, 1)
			if err != nil || !bytes.Contains(page.Items[0].Data.(inbox.Input).Payload, []byte("synthetic")) {
				t.Fatal("external change affected decoded snapshot", err)
			}
			if err = v.Close(); err != nil {
				t.Fatal(err)
			}
			if v.state != nil || s.restoreView.Load() != nil {
				t.Fatal("stale view retained state")
			}
		})
	}
}
func TestRestoreViewCancelReleasesState(t *testing.T) {
	s, id, _ := newRestoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	v, err := s.OpenRestoreView(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = v.Items(t.Context(), id, 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = v.Close(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if v.state != nil || s.restoreView.Load() != nil {
		t.Fatal("cancel retained state")
	}
}
func TestRestoreViewPreservesTornTailAndCorruptionErrors(t *testing.T) {
	for _, mode := range []string{"torn-tail", "committed-corruption"} {
		t.Run(mode, func(t *testing.T) {
			s, id, _ := newRestoreFixture(t)
			path := s.sessionPath(id)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			tail := `{"type":`
			if mode == "committed-corruption" {
				tail += "\n"
			}
			if _, err = f.WriteString(tail); err != nil {
				t.Fatal(err)
			}
			f.Close()
			v, err := s.OpenRestoreView(t.Context(), id)
			if mode == "committed-corruption" {
				if err == nil {
					v.Close()
					t.Fatal("committed corruption accepted")
				}
				if s.restoreView.Load() != nil {
					t.Fatal("failed construction retained view")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			page, err := v.Items(t.Context(), id, 0, 10)
			if err != nil || len(page.Items) != 3 {
				t.Fatal("torn tail behavior changed", err)
			}
			if err = v.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRestoreViewClosesOnFailedAppendAttempt(t *testing.T) {
	s, id, _ := newRestoreFixture(t)
	v, err := s.OpenRestoreView(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// A value that passes the inert state changes but fails record encoding. It
	// must invalidate the old read boundary even if no new record is durable.
	head, size, err := s.loadWriteState(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.append(id, head, size, recordOperation, make(chan int)); err == nil {
		t.Fatal("encoding failure not triggered")
	}
	if _, err = v.Items(t.Context(), id, 0, 1); !errors.Is(err, ErrRestoreViewChanged) {
		t.Fatal("failed attempt retained view", err)
	}
	_ = v.Close()
	if s.restoreView.Load() != nil || v.state != nil {
		t.Fatal("failed append retained history")
	}
}
func TestRestoreViewDoesNotBlockIndependentSession(t *testing.T) {
	s, id, _ := newRestoreFixture(t)
	if _, err := s.Create(t.Context(), "independent"); err != nil {
		t.Fatal(err)
	}
	v, err := s.OpenRestoreView(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = s.AppendInput(ctx, "independent", inbox.Input{ID: "input", Kind: inbox.InputExternal, Payload: jsontext.Value(`{"message":"independent"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Items(t.Context(), id, 0, 1); err != nil {
		t.Fatal("another session invalidated the view", err)
	}
}

var _ sessionstore.HistoryView = (*RestoreView)(nil)

func TestRestoreClonePreservesExactOpaqueValuesAndDetachesMetadata(t *testing.T) {
	raw := jsontext.Value(`{ "n" : 9007199254740993, "escaped":"\u65e5", "array": [] }`)
	original := sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{
		CallID: "synthetic", Status: tool.CallStatus{}, Operations: []operation.Operation{{
			ID: "operation", State: append(jsontext.Value(nil), raw...),
			Execution:   &operation.ExecutionMetadata{Dependencies: []operation.ID{"dependency"}},
			Idempotence: &operation.IdempotencyMetadata{Idempotency: append(jsontext.Value(nil), raw...)},
		}},
	}}
	copy, err := cloneRestoreValue(original)
	if err != nil || !reflect.DeepEqual(original, copy) {
		t.Fatal("clone changed bytes, value types or meaning", err)
	}
	op := copy.Data.(sessionstore.ToolCallStatus).Operations[0]
	op.State[0] = '!'
	op.Execution.Dependencies[0] = "changed"
	op.Idempotence.Idempotency[0] = '!'
	old := original.Data.(sessionstore.ToolCallStatus).Operations[0]
	if !bytes.Equal(old.State, raw) || !bytes.Equal(old.Idempotence.Idempotency, raw) || old.Execution.Dependencies[0] != "dependency" {
		t.Fatal("clone aliases canonical metadata")
	}
	for _, value := range [][]operation.Operation{nil, {}} {
		out, err := cloneRestoreValue(value)
		if err != nil || !reflect.DeepEqual(value, out) {
			t.Fatal("nil/empty changed", err)
		}
	}
}
