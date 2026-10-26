package sessionstore

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func conversationFixture() ConversationEpoch {
	return ConversationEpoch{Number: 1, Provider: "claude-native", TurnID: "synthetic-turn", InputID: "synthetic-input", Attempt: 1, Reason: "continuity", Snapshot: "Quoted public canonical evidence; no current filesystem observation.", Quarantined: []operation.ID{"synthetic-operation"}}
}

func TestConversationEpochCanonicalRoundTrip(t *testing.T) {
	epoch := conversationFixture()
	item := Item{Sequence: 9, Kind: ItemHostRecord, Data: HostRecord{Version: 1, Kind: HostConversationEpoch, Conversation: &epoch}}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal("valid additive epoch record rejected")
	}
	var restored Item
	if json.Unmarshal(encoded, &restored) != nil || !reflect.DeepEqual(restored.Data.(HostRecord).Conversation, &epoch) {
		t.Fatal("canonical epoch record could not be restored")
	}
	// A direct-operation-only Session need not have created a model Turn.
	// The epoch records no fabricated Turn origin in that case.
	epoch.TurnID = ""
	if err := epoch.Validate(); err != nil {
		t.Fatal("epoch invented a mandatory model Turn for direct-operation history")
	}
	legacy := HostRecord{Version: 1, Kind: "configuration", Configuration: []byte(`{}`)}
	encoded, err = json.Marshal(legacy)
	if err != nil || strings.Contains(string(encoded), "Conversation") {
		t.Fatal("legacy host record schema changed when epoch absent")
	}
}

func TestConversationEpochRejectsMalformedOrPrivateData(t *testing.T) {
	for name, mutate := range map[string]func(*ConversationEpoch){
		"number":               func(e *ConversationEpoch) { e.Number = 0 },
		"provider":             func(e *ConversationEpoch) { e.Provider = "openai-codex" },
		"unsafe_turn":          func(e *ConversationEpoch) { e.TurnID = "unsafe\nturn" },
		"missing_input":        func(e *ConversationEpoch) { e.InputID = "" },
		"zero_attempt":         func(e *ConversationEpoch) { e.Attempt = 0 },
		"too_many_attempts":    func(e *ConversationEpoch) { e.Attempt = 4 },
		"unknown_reason":       func(e *ConversationEpoch) { e.Reason = "provider-selected-grant" },
		"missing_snapshot":     func(e *ConversationEpoch) { e.Snapshot = "" },
		"large_snapshot":       func(e *ConversationEpoch) { e.Snapshot = strings.Repeat("x", MaxConversationSnapshotBytes+1) },
		"invalid_utf8":         func(e *ConversationEpoch) { e.Snapshot = string([]byte{0xff}) },
		"terminal_control":     func(e *ConversationEpoch) { e.Snapshot = "synthetic\x1b[1m" },
		"credential":           func(e *ConversationEpoch) { e.Snapshot = "Authorization: Bearer SYNTHETIC_CREDENTIAL_ONLY" },
		"missing_operation":    func(e *ConversationEpoch) { e.Quarantined = []operation.ID{""} },
		"duplicate_operation":  func(e *ConversationEpoch) { e.Quarantined = []operation.ID{"same", "same"} },
		"oversized_operations": func(e *ConversationEpoch) { e.Quarantined = make([]operation.ID, 129) },
	} {
		t.Run(name, func(t *testing.T) {
			epoch := conversationFixture()
			mutate(&epoch)
			err := epoch.Validate()
			if err == nil || strings.Contains(err.Error(), "SYNTHETIC_CREDENTIAL_ONLY") || strings.Contains(err.Error(), "unsafe") {
				t.Fatal("malformed/private epoch accepted or diagnostic leaked its value")
			}
		})
	}
}

func TestConversationEpochIsIsolatedFromOtherHostRecordKinds(t *testing.T) {
	epoch := conversationFixture()
	for _, record := range []HostRecord{
		{Version: 1, Kind: "configuration", Configuration: []byte(`{}`), Conversation: &epoch},
		{Version: 1, Kind: HostConversationEpoch},
		{Version: 1, Kind: HostConversationEpoch, Conversation: &epoch, Configuration: []byte(`{}`)},
		{Version: 1, Kind: HostConversationEpoch, Conversation: &epoch, Inputs: []inbox.ID{"synthetic-input"}},
		{Version: 2, Kind: HostConversationEpoch, Conversation: &epoch},
	} {
		if record.Validate() == nil {
			t.Fatal("epoch record could smuggle unrelated canonical control data")
		}
	}
}
