package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestLegacyRecoveryFieldsAreIgnoredWhenReadingSessions(t *testing.T) {
	for _, version := range []int{2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			const id session.ID = "legacy-session"
			const recorded = `"2026-10-26T00:00:00Z"`
			legacyOperation := `{"ID":"legacy-operation","Type":"value","Version":1,"Status":"awaiting","State":{"Value":"original"},"Dependencies":["prior"],"Idempotency":{"key":"retained"},"PreparationRequired":true,"ExecutionBasis":{"footprint":{"root":"/old/root"}},"RecoveryBasis":[{"ID":"uncertain","Basis":{"old":true}}]}`
			records := []struct {
				kind recordType
				data string
			}{
				{recordSession, fmt.Sprintf(`{"Version":%d,"Session":{"ID":"legacy-session","CreatedAt":%s}}`, version, recorded)},
				{recordItem, `{"Item":{"Sequence":1,"RecordedAt":` + recorded + `,"Kind":"host_record","Data":{"Version":1,"Kind":"model_conversation_epoch","Conversation":{"Number":99,"Provider":"claude-native","Reason":"quarantine","Quarantined":["uncertain"],"UncertainEffects":[{"ID":"uncertain","Basis":{"old":true}}],"Snapshot":"old snapshot"}}}}`},
				{recordItem, `{"Item":{"Sequence":2,"RecordedAt":` + recorded + `,"Kind":"host_record","Data":{"Version":1,"Kind":"model_request_failure","ModelRequestFailure":{"TurnID":"old-turn","HTTPStatus":429}}}}`},
				{recordItem, `{"Item":{"Sequence":3,"RecordedAt":` + recorded + `,"Kind":"turn","Data":{"ID":"turn-1","Type":"regular","NoAutomaticReplay":true}}}`},
				{recordItem, `{"Item":{"Sequence":4,"RecordedAt":` + recorded + `,"Kind":"model_response","Data":{"TurnID":"turn-1","Response":{}}}}`},
				{recordItem, `{"Item":{"Sequence":5,"RecordedAt":` + recorded + `,"Kind":"tool_call_status","Data":{"TurnID":"turn-1","CallID":"call-1","Status":{"WaitingFor":["legacy-operation"]},"InheritedFacts":[` + legacyOperation + `]}},"Operations":[` + legacyOperation + `]}`},
				{recordOperation, `{"Operation":` + legacyOperation + `}`},
			}
			var encoded []byte
			for _, record := range records {
				line, err := encodeRecord(record.kind, jsontext.Value(record.data))
				if err != nil {
					t.Fatal(err)
				}
				encoded = append(encoded, line...)
			}
			if _, _, err := decodeLog(encoded); err != nil {
				t.Fatal("legacy log decode:", err)
			}
			directory := t.TempDir()
			path := filepath.Join(directory, string(id)+".session.jsonl")
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := New(directory)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := store.Resume(t.Context(), id)
			if err != nil {
				t.Fatal("legacy resume:", err)
			}
			view, err := store.OpenRestoreView(t.Context(), id)
			if err != nil {
				t.Fatal("legacy restore view:", err)
			}
			defer view.Close()
			fromView, err := view.Resume(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []sessionstore.ResumeState{restored, fromView} {
				if len(state.Operations) != 1 {
					t.Fatal("legacy operation lost", state)
				}
				op := state.Operations[0]
				if op.ID != "legacy-operation" || op.Status != operation.StatusAwaiting || len(op.DependencyIDs()) != 1 || op.DependencyIDs()[0] != "prior" || string(op.Idempotency()) != `{"key":"retained"}` {
					t.Fatal("current operation metadata lost", op)
				}
			}
			page, err := view.Items(t.Context(), id, sessionstore.BeforeFirst, 20)
			if err != nil || len(page.Items) != 5 {
				t.Fatal("legacy canonical history lost", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, encoded) {
				t.Fatal("reading changed the existing session", err)
			}
		})
	}
}
