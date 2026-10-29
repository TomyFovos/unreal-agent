package tui

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func TestComposerSelectAllEditing(t *testing.T) {
	for _, initial := range []Key{{Text: "猫👩🏽‍💻é"}, {Text: "first\nsecond\nthird"}, {Text: strings.Repeat("pasted\n", 40), Paste: true}} {
		for _, action := range []Key{{Text: "replacement"}, {Name: "backspace"}, {Name: "delete"}, {Text: "new\npaste\nbody", Paste: true}} {
			var e Editor
			e.Apply(initial)
			e.Apply(Key{Name: "selectall"})
			if !e.Selected {
				t.Fatal("not selected")
			}
			e.Apply(action)
			want := action.Text
			if got := e.Expanded(); got != want || e.Selected {
				t.Fatalf("got %q selected=%v, want %q", got, e.Selected, want)
			}
			if want == "" && (e.Pasted || len(e.pastes) > 0) {
				t.Fatal("erased selection retained paste state")
			}
		}
		for _, move := range []string{"left", "right", "up", "down", "home", "end", "escape"} {
			var e Editor
			e.Apply(initial)
			before := e.Expanded()
			e.Apply(Key{Name: "selectall"})
			e.Apply(Key{Name: move})
			if e.Selected || e.Expanded() != before {
				t.Fatalf("%s damaged selection contents", move)
			}
		}
	}
}

func TestComposerSelectAllRenderingAndSecret(t *testing.T) {
	var e Editor
	e.Apply(Key{Text: "secret猫\nsecond"})
	e.Apply(Key{Name: "selectall"})
	for _, secret := range []bool{false, true} {
		u := UIState{Input: e.Text(), Cursor: e.cursor, Selected: e.Selected, Secret: secret}
		rows, _, _, _, _ := composer(Snapshot{}, u, makeLayout(80, 24))
		selected := 0
		var text strings.Builder
		for _, row := range rows {
			for _, cell := range row {
				if cell.style.reverse {
					selected++
				}
			}
			text.WriteString(row.paint(Theme{Depth: ColorTrue}))
		}
		if selected == 0 {
			t.Fatal("no reverse selection")
		}
		if secret && (strings.Contains(text.String(), "secret") || strings.Contains(text.String(), "second") || strings.Contains(e.Display(true), "secret")) {
			t.Fatal("selected secret exposed")
		}
	}
	for _, position := range []int{0, len("secret"), len(e.Text())} {
		u := UIState{Input: e.Text(), Cursor: position, Selected: true, Secret: true}
		_, row, col, _, _ := composer(Snapshot{}, u, makeLayout(80, 24))
		expected := 2 + min(64, uniseg.GraphemeClusterCount(e.Text()[:position]))
		if row != 0 || col != expected {
			t.Fatalf("secret caret at %d: %d,%d; want 0,%d", position, row, col, expected)
		}
	}
	e.Apply(Key{Text: "next"})
	if e.Display(true) != "API key> ****" {
		t.Fatal("secret replacement", e.Display(true))
	}
	var d Decoder
	keys := d.Feed([]byte("\x01\x1b[3~\x1b[H"))
	if len(keys) != 3 || keys[0].Name != "selectall" || keys[1].Name != "delete" || keys[2].Name != "home" {
		t.Fatal(keys)
	}
}

func TestComposerSelectAllRunWithSlashMenuAndArrows(t *testing.T) {
	f := newFake()
	keys := make(chan Key, 16)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	keys <- Key{Text: "/hel"}
	keys <- Key{Name: "selectall"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Type/paste replace") })
	keys <- Key{Name: "up"}
	keys <- Key{Text: "x"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "/helx") })
	keys <- Key{Name: "selectall"}
	keys <- Key{Text: "replaced"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "replaced") && !strings.Contains(out.text(), "/helx") })
	keys <- Key{Name: "selectall"}
	keys <- Key{Name: "escape"}
	keys <- Key{Text: " appended"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "replaced appended") })
}
