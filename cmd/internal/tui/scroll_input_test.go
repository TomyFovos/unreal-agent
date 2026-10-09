package tui

import (
	"strings"
	"testing"
)

func TestScrollDecoderMouseWheelAndHistoryKeys(t *testing.T) {
	for _, tc := range []struct{ wire, name string }{
		{"\x1b[<64;12;5M", "wheelup"}, {"\x1b[<65;12;5M", "wheeldown"},
		{"\x1b[1;5H", "historyhome"}, {"\x1b[1;5F", "historyend"},
		{"\x1b[5~", "pageup"}, {"\x1b[6~", "pagedown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Decoder
			var keys []Key
			for _, b := range []byte(tc.wire) {
				keys = append(keys, d.Feed([]byte{b})...)
			}
			if len(keys) != 1 || keys[0].Name != tc.name || keys[0].Text != "" {
				t.Fatalf("wire lost or treated as composer input: %+v", keys)
			}
		})
	}
}

func TestScrollDecoderDoesNotExecutePastedMouseOrHistoryKeys(t *testing.T) {
	var d Decoder
	keys := d.Feed([]byte("\x1b[200~\x1b[<64;12;5M\x1b[1;5H\x1b[201~"))
	if len(keys) != 0 {
		t.Fatal(keys)
	}
}

func TestScrollMouseMalformedClickReleaseAndModifiers(t *testing.T) {
	for _, wire := range []string{"\x1b[<0;12;5M", "\x1b[<64;12;5m", "\x1b[<96;12;5M", "\x1b[<64;0;5M", "\x1b[<64;-1;5M", "\x1b[<64;99999;5M", "\x1b[<64;1;2;3M"} {
		if key, ok := mouseWheel(wire); ok || key.Text != "" {
			t.Fatal("unsupported mouse report acquired input semantics", wire, key)
		}
	}
	key, ok := mouseWheel("\x1b[<68;12;5M")
	if !ok || key.Name != "wheelup" || key.MouseX != 12 || key.MouseY != 5 {
		t.Fatal("modified wheel decoding", key)
	}
}

func TestScrollWheelReachesConversationWithoutSubmittingComposer(t *testing.T) {
	f := conversationFixture("FIRST\n" + strings.Repeat("long 日本語 👩🏽‍💻 output\n", 100) + "LAST")
	keys := make(chan Key, 32)
	out := &screenObserver{}
	theme := Theme{Plain: true}
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "LAST") })
	keys <- Key{Text: "unsent composer"}
	keys <- Key{Name: "wheelup"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "scrollback:") })
	keys <- Key{Name: "historyhome"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "FIRST") })
	keys <- Key{Name: "historyend"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "LAST") && !strings.Contains(out.text(), "scrollback:")
	})
	if !strings.Contains(out.text(), "unsent composer") {
		t.Fatal("scroll changed input")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("scroll submitted input")
	}
}
