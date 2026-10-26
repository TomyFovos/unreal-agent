package contextbuilder

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// A child's ready notice repeats the handle its start returned; it may arrive
// after the child ended, so the model never reads it. Other peer messages do.
func TestReadyNoticeStaysOutOfTheRequest(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	for kind, text := range map[string]string{"ready": "Child is ready; operation handle op-1", "message": "worker finished the tests"} {
		payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "parent", Handle: "op-1", Kind: kind, Text: text})
		f.item(t, sessionstore.ItemInput, inbox.Input{ID: inbox.ID(kind), Kind: inbox.InputPeer, Payload: payload})
	}
	built, err := f.b.Build()
	if err != nil {
		t.Fatal(err)
	}
	text := requestText(built)
	if strings.Contains(text, "Child is ready") || !strings.Contains(text, "worker finished the tests") {
		t.Fatalf("request = %q", text)
	}
}
