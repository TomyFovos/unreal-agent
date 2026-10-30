package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func recoveryHeads(ops []operation.Operation) map[operation.ID]operation.Operation {
	out := make(map[operation.ID]operation.Operation)
	for _, op := range ops {
		out[op.ID] = operation.Operation{ID: op.ID, Type: op.Type, ToolName: op.ToolName, Status: op.Status}
	}
	return out
}

func TestRecoveryScanMatchesCanonicalOperations(t *testing.T) {
	paths := []string{"testdata/golden-session.session.jsonl", "testdata/golden-fork.session.jsonl"}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, _, wantErr := decodeLog(encoded)
			if wantErr != nil {
				t.Fatal(wantErr)
			}
			got, ok := recoveryOperations(t.Context(), append(encoded, []byte(`{"torn":`)...))
			if !ok {
				t.Fatal("canonical fixture did not use the metadata scan")
			}
			if !reflect.DeepEqual(recoveryHeads(got), recoveryHeads(want.Operations)) {
				t.Fatalf("scan=%+v canonical=%+v", got, want.Operations)
			}
		})
	}
}

func TestRecoveryUnsupportedHistoryKeepsCanonicalFailure(t *testing.T) {
	for _, name := range []string{"legacy-session", "soft-stop-session"} {
		t.Run(name, func(t *testing.T) {
			encoded, err := os.ReadFile(filepath.Join("testdata", name+sessionFileSuffix))
			if err != nil {
				t.Fatal(err)
			}
			_, _, wantErr := decodeLog(encoded)
			if wantErr == nil {
				t.Fatal("fixture must exercise the canonical refusal")
			}
			store, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.sessionPath(session.ID(name)), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			_, gotErr := store.RecoveryOperations(t.Context(), session.ID(name))
			if gotErr == nil || !strings.Contains(gotErr.Error(), wantErr.Error()) {
				t.Fatal("cleanup changed canonical error", gotErr, wantErr)
			}
		})
	}
}

func TestRecoveryScanUsesFirstToolStatusAndLatestCheckpoint(t *testing.T) {
	s := newStoredState("recovery", stateCreatedAt)
	if err := s.appendTurn(session.Turn{ID: "turn"}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	op := validOperation("child", operation.StatusReady)
	op.ToolName = "SubagentStart"
	status := sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}}
	if err := s.appendToolCallStatus(status, []operation.Operation{op}, stateUpdatedAt); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeInitialLog(s.Snapshot.Session, s.Items)
	if err != nil {
		t.Fatal(err)
	}
	op.Status = operation.StatusAwaiting
	encoded = append(encoded, mustRecord(t, recordOperation, operationRecord{Operation: op})...)
	// Canonical replay ignores Operations on an already recorded call status.
	ignored := op
	ignored.Status = operation.StatusCompleted
	encoded = append(encoded, mustRecord(t, recordItem, itemRecord{Item: sessionstore.Item{Sequence: 3, RecordedAt: stateUpdatedAt, Kind: sessionstore.ItemToolCallStatus, Data: status}, Operations: []operation.Operation{ignored}})...)
	want, _, err := decodeLog(encoded)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := recoveryOperations(t.Context(), encoded)
	if !ok || !reflect.DeepEqual(recoveryHeads(got), recoveryHeads(want.Operations)) {
		t.Fatalf("scan=%+v canonical=%+v", got, want.Operations)
	}
}

func TestTakeRestoreOwnershipAndBorrowedHistoryInvalidation(t *testing.T) {
	s, id, _ := newRestoreFixture(t)
	v, err := s.OpenRestoreView(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := v.Take(t.Context(), id)
	if err != nil || len(items) != 3 {
		t.Fatal("take", len(items), err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if v.state != nil || s.restoreView.Load() != nil {
		t.Fatal("take retained decoded log")
	}
	borrowed, err := s.OpenHistoryItems(t.Context(), id, items)
	if err != nil {
		t.Fatal(err)
	}
	page, err := borrowed.Items(t.Context(), id, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	input := page.Items[0].Data.(inbox.Input)
	input.Payload[0] = '!'
	if !bytes.HasPrefix(items[0].Data.(inbox.Input).Payload, []byte("{")) {
		t.Fatal("page changed Host-owned history")
	}
	if err := s.AppendInput(t.Context(), id, inbox.Input{ID: "new", Kind: inbox.InputExternal, Payload: jsontext.Value(`"new"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowed.Items(t.Context(), id, 1, 1); err != ErrRestoreViewChanged {
		t.Fatal("raw append did not invalidate memory view", err)
	}
	_ = borrowed.Close()
	page, err = s.Items(t.Context(), id, 0, 10)
	if err != nil || len(page.Items) != 4 {
		t.Fatal("canonical history after take/append", page, err)
	}
}
