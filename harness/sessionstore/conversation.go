package sessionstore

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

const HostConversationEpoch = "model_conversation_epoch"

const MaxConversationSnapshotBytes = 24 << 10

// ConversationEpoch changes only the model's disposable conversation projection.
// It grants no permission, completes no Operation and does not acknowledge or
// replace canonical tool requests or receipts. Snapshot is bounded quoted public
// evidence, never provider-private thinking, credentials or a workspace backup.
type ConversationEpoch struct {
	Number           uint64
	Provider         string
	TurnID           session.TurnID
	InputID          string
	Attempt          uint32
	Reason           string
	Snapshot         string
	Quarantined      []operation.ID              `json:",omitzero"`
	UncertainEffects []operation.UncertainEffect `json:",omitzero"`
}

func (e ConversationEpoch) Validate() error {
	if e.Number == 0 || e.Provider != "claude-native" || e.Attempt == 0 || e.Attempt > 3 || e.TurnID != "" && !conversationIdentity(string(e.TurnID)) || !conversationIdentity(e.InputID) {
		return errors.New("invalid model conversation epoch identity")
	}
	if e.Reason != "continuity" && e.Reason != "quarantine" && e.Reason != "reprojection" && e.Reason != "strict_restore" {
		return errors.New("invalid model conversation epoch reason")
	}
	if len(e.Snapshot) == 0 || len(e.Snapshot) > MaxConversationSnapshotBytes || !utf8.ValidString(e.Snapshot) || strings.ContainsAny(e.Snapshot, "\x00\x1b") || secretguard.Sensitive(e.Snapshot) {
		return errors.New("invalid model conversation snapshot")
	}
	if len(e.Quarantined) > 128 {
		return errors.New("too many quarantined operations")
	}
	seen := make(map[operation.ID]bool, len(e.Quarantined))
	for _, id := range e.Quarantined {
		if !conversationIdentity(string(id)) || seen[id] {
			return errors.New("invalid quarantined operation identity")
		}
		seen[id] = true
	}
	if len(e.UncertainEffects) > len(e.Quarantined) {
		return errors.New("unbound uncertain execution evidence")
	}
	evidenceSeen := map[operation.ID]bool{}
	total := 0
	for _, effect := range e.UncertainEffects {
		total += len(effect.Basis)
		if !seen[effect.ID] || evidenceSeen[effect.ID] || len(effect.Basis) == 0 || len(effect.Basis) > 256<<10 || total > 1<<20 || !effect.Basis.IsValid() {
			return errors.New("invalid bounded uncertain execution evidence")
		}
		evidenceSeen[effect.ID] = true
	}
	return nil
}

func conversationIdentity(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
			return false
		}
	}
	return true
}
