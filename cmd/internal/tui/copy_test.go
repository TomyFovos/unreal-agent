package tui

import (
	"encoding/base64"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestCopyPickerCanonicalStatementsAndSafety(t *testing.T) {
	f := conversationFixture("first agent\nsecond line")
	add := func(kind sessionstore.ItemKind, data any) {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(len(f.view.History.Items) + 1), Kind: kind, Data: data})
	}
	peer, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "s", Handle: "h", Kind: "ended", Text: "private peer"})
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputPeer, Payload: peer})
	response := func(turn session.TurnID, texts ...string) {
		r := llm.Response{}
		for _, text := range texts {
			r.Output = append(r.Output, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}})
		}
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: r})
	}
	response("background", "background work")
	response("background", "【報告】public report\nmore report")
	add(sessionstore.ItemTurn, session.Turn{ID: "compact", Type: session.TurnCompaction})
	response("compact", "private compaction")
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"latest user\nsecond user line"`)})
	response("latest", "latest agent", "another statement")
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"password=protected-user"`)})
	response("sensitive", "api_key=sk-protected1234")
	choices, err := CopyChoices(t.Context(), f, "s", throughHistory(f))
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, c := range choices {
		labels = append(labels, c.Role+": "+c.Preview)
	}
	want := []string{"Agent: another statement", "Agent: latest agent", "You: latest user", "Agent: 【報告】public report", "Agent: first agent", "You: public user input"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("choices=%q", labels)
	}
	p := copyPicker(choices)
	if p.Options[0] != "すべて" || p.Selection != 0 {
		t.Fatal("all must be first")
	}
	p.move(1)
	text, err := CopyMessagesText(t.Context(), f, "s", p.copySelection())
	if err != nil || text != "another statement" {
		t.Fatal("single copy", text, err)
	}
	p.toggleCopy()
	p.move(5)
	p.toggleCopy()
	text, err = CopyMessagesText(t.Context(), f, "s", p.copySelection())
	if err != nil || text != "## You\n\npublic user input\n\n## Agent\n\nanother statement" {
		t.Fatal("multiple copy order", text, err)
	}
	p.Selection = 0
	p.toggleCopy()
	all, err := CopyMessagesText(t.Context(), f, "s", p.copySelection())
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"background work", "private peer", "private compaction", "protected-user", "sk-protected", "private tool", "hidden private reasoning"} {
		if strings.Contains(all, hidden) || strings.Contains(strings.Join(p.Options, "\n"), hidden) {
			t.Fatal("private content copied", hidden)
		}
	}
	for _, body := range []string{"first agent\nsecond line", "latest user\nsecond user line", "another statement", "【報告】public report\nmore report"} {
		if !strings.Contains(all, body) {
			t.Fatal("all lost message", body)
		}
	}
	if _, err := CopyMessagesText(t.Context(), f, "s", []MessageChoice{{Sequence: throughHistory(f), Part: 0}}); err == nil {
		t.Fatal("forged protected reference copied")
	}
}

func TestCopyPickerKeysWriteSelectedMessagesThroughOSC52(t *testing.T) {
	for _, mode := range []string{"single", "multiple", "all"} {
		t.Run(mode, func(t *testing.T) {
			f := conversationFixture("earlier agent", "latest agent")
			keys, out, done := make(chan Key, 16), &screenObserver{}, make(chan error, 1)
			go func() {
				done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}})
			}()
			defer func() {
				keys <- Key{Name: "detach"}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}()
			waitFor(t, func() bool { return strings.Contains(out.text(), "latest agent") })
			keys <- Key{Text: "/copy"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Space select") })
			switch mode {
			case "single":
				keys <- Key{Name: "down"}
			case "multiple":
				keys <- Key{Name: "down"}
				keys <- Key{Text: " "}
				keys <- Key{Name: "down"}
				keys <- Key{Name: "down"}
				keys <- Key{Text: " "}
			}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.rawText(), "\x1b]52;c;") })
			raw := strings.Split(out.rawText(), "\x1b]52;c;")[1]
			encoded, _, _ := strings.Cut(raw, "\x07")
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if mode == "single" && text != "latest agent" {
				t.Fatal("single", text)
			}
			if mode == "multiple" && (strings.Contains(text, "public user input") || strings.Index(text, "earlier agent") > strings.Index(text, "latest agent")) {
				t.Fatal("multiple", text)
			}
			if mode == "all" && (strings.Count(text, "public user input") != 2 || !strings.Contains(text, "earlier agent") || !strings.Contains(text, "latest agent")) {
				t.Fatal("all", text)
			}
			if strings.Contains(text, "private") {
				t.Fatal("private copy", text)
			}
		})
	}
}
