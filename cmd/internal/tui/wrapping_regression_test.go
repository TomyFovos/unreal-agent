package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestDecoderAndWrapPreservesLongWhitespace(t *testing.T) {
	// The saved fuzz seed is a bell, period, 2463 spaces, hash and bell.
	var decoder Decoder
	var editor Editor
	input := "\a." + strings.Repeat(" ", 2463) + "#\a"
	for _, key := range decoder.Feed([]byte(input)) {
		editor.Apply(key)
	}
	if want := "." + strings.Repeat(" ", 2463) + "#"; editor.Text() != want || editor.cursor != len(want) {
		t.Fatal("decoder/editor changed the whitespace or cursor")
	}
	want := append([]string{"> ." + strings.Repeat(" ", 14)}, slices.Repeat([]string{strings.Repeat(" ", 17)}, 144)...)
	want = append(want, " #")
	if got := Wrap(editor.Display(false), 17); !slices.Equal(got, want) {
		t.Fatalf("whitespace wrapping changed: got %d rows, want %d", len(got), len(want))
	}
}

func TestWrapPreservesWhitespaceAndExtendedGraphemes(t *testing.T) {
	for _, test := range []struct {
		text string
		want []string
	}{
		{text: "e\u0301  👩🏽‍💻  猫", want: []string{"e\u0301  ", "👩🏽‍💻 ", " 猫"}},
		{text: ". \t#", want: []string{".  ", "   ", "#"}},
		{text: "🇯🇵🇺🇸", want: []string{"🇯🇵", "🇺🇸"}},
	} {
		if got := Wrap(test.text, 3); !slices.Equal(got, test.want) {
			t.Errorf("Wrap(%q, 3) = %q, want %q", test.text, got, test.want)
		}
	}
}

func TestEditorMovesAndDeletesExtendedGraphemes(t *testing.T) {
	clusters := []string{"e\u0301", "👩🏽‍💻", "🇯🇵", "日"}
	var decoder Decoder
	var editor Editor
	for _, key := range decoder.Feed([]byte(strings.Join(clusters, ""))) {
		editor.Apply(key)
	}
	for i := len(clusters) - 1; i >= 0; i-- {
		editor.Apply(Key{Name: "left"})
		if want := len(strings.Join(clusters[:i], "")); editor.cursor != want {
			t.Fatalf("left cursor = %d, want %d", editor.cursor, want)
		}
	}
	for i := 1; i <= len(clusters); i++ {
		editor.Apply(Key{Name: "right"})
		if want := len(strings.Join(clusters[:i], "")); editor.cursor != want {
			t.Fatalf("right cursor = %d, want %d", editor.cursor, want)
		}
	}
	for i := len(clusters) - 1; i >= 0; i-- {
		editor.Apply(Key{Name: "backspace"})
		if want := strings.Join(clusters[:i], ""); editor.Text() != want || editor.cursor != len(want) {
			t.Fatalf("backspace = %q at %d, want %q", editor.Text(), editor.cursor, want)
		}
	}
	editor.Apply(Key{Text: strings.Join(clusters, "")})
	editor.Apply(Key{Name: "home"})
	for i := 1; i <= len(clusters); i++ {
		editor.Apply(Key{Name: "delete"})
		if want := strings.Join(clusters[i:], ""); editor.Text() != want || editor.cursor != 0 {
			t.Fatalf("delete = %q at %d, want %q at 0", editor.Text(), editor.cursor, want)
		}
	}
}

func BenchmarkDecoderAndWrapWhitespace(b *testing.B) {
	for _, spaces := range []int{1024, 4096, 16384} {
		input := []byte("\a." + strings.Repeat(" ", spaces) + "#\a")
		b.Run(strconv.Itoa(spaces), func(b *testing.B) {
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				var decoder Decoder
				var editor Editor
				for _, key := range decoder.Feed(input) {
					editor.Apply(key)
				}
				_ = Wrap(editor.Display(false), 17)
			}
		})
	}
}
