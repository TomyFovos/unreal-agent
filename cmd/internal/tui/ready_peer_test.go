package tui

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Starting a child in a normal turn sends a ready note; the rest of the
// user's turn stays visible in the chat and copyable.
func TestReadyNoteKeepsTheUsersTurnInChatAndCopy(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Entries = []Entry{
		{Role: "you", Text: "調べて"},
		{Role: "peer", PeerID: "child", PeerKind: "ready", Text: "Child is ready"},
		{Role: "peer", PeerID: "child", PeerKind: "progress", Text: "halfway"},
		{Role: "agent", Text: "作業者に渡しました。結果を待ちます。"},
	}
	if got := chatText(s, u, 120); !strings.Contains(got, "● 作業者に渡しました。") {
		t.Fatalf("user's turn folded into background work:\n%s", got)
	}
	if backgroundTurn(s) {
		t.Fatal("ready/progress notes made the user's turn a background turn")
	}

	f := conversationFixture("first agent")
	add := func(kind sessionstore.ItemKind, data any) {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(len(f.view.History.Items) + 1), Kind: kind, Data: data})
	}
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"調べて"`)})
	ready, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "s", Handle: "h", Kind: "ready", Text: "Child is ready"})
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputPeer, Payload: ready})
	add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "user", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "作業者に渡しました。"}}}}})
	choices, err := CopyChoices(t.Context(), f, "s", throughHistory(f))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(choices, func(c MessageChoice) bool { return c.Role == "Agent" && c.Preview == "作業者に渡しました。" }) {
		t.Fatalf("reply after a ready note missing from /copy: %+v", choices)
	}
}
