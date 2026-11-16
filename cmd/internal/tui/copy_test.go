package tui

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
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
	t.Parallel()
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
	t.Parallel()
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

func TestCopyPickerReadsRecentPageAndSelectedSequences(t *testing.T) {
	t.Parallel()
	f := lazyFixture(3000)
	p, err := loadCopyPicker(t.Context(), f, "s", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if f.inspects != 0 || f.backwards != 1 || f.backwardItems != historyPageItems || len(p.Messages) != historyPageItems || !p.CopyOlder {
		t.Fatalf("opening copy replayed history: forward=%d backward=%d items=%d choices=%d", f.inspects, f.backwards, f.backwardItems, len(p.Messages))
	}
	if p.Messages[0].Sequence != 3000 || p.Messages[127].Sequence != 2873 {
		t.Fatal("recent choices are not newest first")
	}
	p.Selection = 1
	text, err := copyPickerText(t.Context(), f, "s", p)
	if err != nil || text != "entry-2999" {
		t.Fatal("selected text", text, err)
	}
	if f.inspects != 0 || f.backwards != 2 || f.backwardItems != historyPageItems+1 {
		t.Fatal("selected copy reread unrelated items", f.inspects, f.backwards, f.backwardItems)
	}
	// Repeated parts in one response still require only one canonical read.
	_, err = CopyMessagesText(t.Context(), f, "s", []MessageChoice{p.Messages[0], p.Messages[0], p.Messages[127]})
	if err != nil || f.backwardItems != historyPageItems+3 {
		t.Fatal("sequences were not deduplicated", err, f.backwardItems)
	}
}

func TestCopyPickerOlderPagesKeepSelectionOrderAndAll(t *testing.T) {
	t.Parallel()
	f := lazyFixture(3000)
	p, err := loadCopyPicker(t.Context(), f, "s", 3000)
	if err != nil {
		t.Fatal(err)
	}
	p.Selection = 1
	p.toggleCopy()
	p.Selection = len(p.Options) - 2
	n, err := loadOlderCopyPicker(t.Context(), f, "s", p, 9)
	if err != nil {
		t.Fatal(err)
	}
	if f.inspects != 0 || f.backwardItems != 256 || len(n.Messages) != 256 || n.Selection != p.Selection+9 || !n.Checked[1] || len(p.Messages) != 128 {
		t.Fatal("scroll lost a selection or reloaded history")
	}
	for i, choice := range n.Messages {
		if choice.Sequence != sessionstore.Sequence(3000-i) {
			t.Fatalf("page overlap/gap at %d: %d", i, choice.Sequence)
		}
	}
	n.toggleCopy()
	got, err := copyPickerText(t.Context(), f, "s", n)
	if err != nil || got != "## Agent\n\nentry-2864\n\n## Agent\n\nentry-2999" {
		t.Fatal("cross-page selection text/order", got, err)
	}
	// "All" includes the omitted prefix, even before scrolling to it.
	// Appends after opening the picker remain outside its captured boundary.
	f.mu.Lock()
	f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 3001, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "appended after opening"}}}}}})
	f.mu.Unlock()
	p.Selection = 0
	p.toggleCopy()
	got, err = copyPickerText(t.Context(), f, "s", p)
	if err != nil {
		t.Fatal(err)
	}
	choices, err := CopyChoices(t.Context(), f.pagedTranscriptClient, "s", 3000)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CopyMessagesText(t.Context(), f.pagedTranscriptClient, "s", choices)
	if err != nil || got != want || strings.Count(got, "entry-") != 3000 || strings.Contains(got, "appended after opening") {
		t.Fatal("all differs from the original full-history copy", err)
	}
	for n.CopyOlder {
		n, err = loadOlderCopyPicker(t.Context(), f, "s", n, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(n.Messages) != 3000 || n.Messages[2999].Sequence != 1 {
		t.Fatal("oldest messages missing")
	}
}

func TestCopyPickerPageDownLoadsOlderMessagesThroughOSC52(t *testing.T) {
	t.Parallel()
	f := lazyFixture(300)
	keys, out, done := make(chan Key, 32), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Size: func() (int, int) { return 80, 24 }, Theme: &Theme{Plain: true}})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "entry-0299") })
	keys <- Key{Text: "/copy"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Space select") })
	// 22 page steps of six options cross the initial 128-item window.
	for range 22 {
		keys <- Key{Name: "pagedown"}
	}
	waitFor(t, func() bool { return strings.Contains(out.text(), "entry-0168") })
	keys <- Key{Text: " "}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.rawText(), "\x1b]52;c;") })
	raw := strings.Split(out.rawText(), "\x1b]52;c;")[1]
	encoded, _, _ := strings.Cut(raw, "\x07")
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(body) != "entry-0168" {
		t.Fatal("scrolled choice copied incorrectly", string(body), err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.backwardItems != 3*historyPageItems+1 {
		t.Fatal("scroll/copy reread unrelated history", f.backwardItems)
	}
}

func TestCopyPickerAllEmptyAndSingleMessage(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1} {
		f := lazyFixture(n)
		p, err := loadCopyPicker(t.Context(), f, "s", sessionstore.Sequence(n))
		if err != nil {
			t.Fatal(err)
		}
		text, err := copyPickerText(t.Context(), f, "s", p)
		if n == 0 && err == nil || n == 1 && (err != nil || text != "entry-0000") {
			t.Fatal("all empty/single behavior changed", n, text, err)
		}
	}
}

func TestCopyPagesAndDirectBodiesMatchFullHistoryFilters(t *testing.T) {
	t.Parallel()
	var hiddenSequences []sessionstore.Sequence
	client, through, _ := copySessionFixture(t, func(record func(sessionstore.ItemKind, any)) {
		var sequence sessionstore.Sequence
		add := func(kind sessionstore.ItemKind, data any) {
			sequence++
			record(kind, data)
		}
		var turn session.TurnID
		var turnType session.TurnType
		responded, responses := false, 0
		inputNumber := 0
		input := func(kind inbox.InputKind, text string) {
			inputNumber++
			payload, _ := json.Marshal(text)
			if kind == inbox.InputPeer {
				payload, _ = json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "copy-bench", Handle: "h", Kind: text, Text: "peer message"})
			}
			add(sessionstore.ItemInput, inbox.Input{ID: inbox.ID(fmt.Sprint(inputNumber)), Kind: kind, Payload: payload})
		}
		startTurn := func(id session.TurnID, kind session.TurnType) {
			add(sessionstore.ItemTurn, session.Turn{ID: id, PreviousTurnID: turn, Type: kind})
			turn, turnType, responded = id, kind, false
		}
		response := func(texts ...string) sessionstore.Sequence {
			responses++
			if responded {
				startTurn(session.TurnID(fmt.Sprintf("extra-%d", responses)), turnType)
			}
			r := llm.Response{}
			for _, text := range texts {
				r.Output = append(r.Output, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}})
			}
			add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: r})
			responded = true
			return sequence
		}
		input(inbox.InputExternal, "最初の質問\n日本語 👩🏽‍💻")
		startTurn("first", session.TurnRegular)
		response("first public statement", "second public statement\nmore text")
		input(inbox.InputPeer, "ready")
		input(inbox.InputPeer, "progress")
		// Each extra response adds a Turn and ModelResponse. Cross a whole
		// page in each context, including pages with no copyable statements.
		const responsesAcrossPage = historyPageItems/2 + 1
		for range responsesAcrossPage {
			response("public after quiet note")
		}
		startTurn("compact", session.TurnCompaction)
		for range responsesAcrossPage {
			seq := response("private compaction")
			if len(hiddenSequences) == 0 {
				hiddenSequences = append(hiddenSequences, seq)
			}
		}
		startTurn("background", session.TurnRegular)
		input(inbox.InputPeer, "ended")
		for range responsesAcrossPage {
			seq := response("private background")
			if len(hiddenSequences) == 1 {
				hiddenSequences = append(hiddenSequences, seq)
			}
		}
		response("【報告】public report\nfull report")
		input(inbox.InputExternal, "latest question")
		startTurn("latest", session.TurnRegular)
		response("latest commentary", "latest final\nsecond line")
		hiddenSequences = append(hiddenSequences, response("visible part must also be excluded", "api_key=sk-protected1234"))
	})
	// Expose only the legacy forward interface as the unchanged full-scan oracle.
	legacy := struct{ Reader }{client}
	choices, err := CopyChoices(t.Context(), legacy, client.owner.ID, through)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CopyMessagesText(t.Context(), legacy, client.owner.ID, choices)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadCopyPicker(t.Context(), client, client.owner.ID, through)
	if err != nil {
		t.Fatal(err)
	}
	got, err := copyPickerText(t.Context(), client, client.owner.ID, p)
	if err != nil || got != want {
		t.Fatal("all changed across omitted context", err)
	}
	for _, hidden := range []string{"private compaction", "private background", "visible part must also", "sk-protected"} {
		if strings.Contains(got, hidden) {
			t.Fatal("hidden text copied", hidden)
		}
	}
	for p.CopyOlder {
		p, err = loadOlderCopyPicker(t.Context(), client, client.owner.ID, p, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(p.Messages, choices) {
		t.Fatal("paged choices differ from full-history choices")
	}
	for _, choice := range p.Messages {
		got, err = CopyMessagesText(t.Context(), client, client.owner.ID, []MessageChoice{choice})
		if err != nil {
			t.Fatal(err)
		}
		want, err = CopyMessagesText(t.Context(), legacy, client.owner.ID, []MessageChoice{choice})
		if err != nil || got != want {
			t.Fatal("direct body differs from full scan", choice.Sequence, err)
		}
	}
	// Forging a canonical reference still cannot copy hidden responses.
	for _, sequence := range hiddenSequences {
		if _, err := CopyMessagesText(t.Context(), client, client.owner.ID, []MessageChoice{{Sequence: sequence}}); err == nil {
			t.Fatal("hidden reference copied", sequence)
		}
	}
	// Multiple parts of one response use canonical part order, despite selection order.
	got, err = CopyMessagesText(t.Context(), client, client.owner.ID, choices[:2])
	if err != nil || got != "## Agent\n\nlatest commentary\n\n## Agent\n\nlatest final\nsecond line" {
		t.Fatal("part order changed", got, err)
	}
}
