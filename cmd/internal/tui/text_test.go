package tui

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func TestTailTruncatesAtGraphemeCellBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  string
	}{
		{"abcdef", 4, "abc…"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 1, "…"},
		{"abcdef", 0, ""},
		{"abcdef", -1, ""},
		{"", 1, ""},
		{"日本語の題名", 7, "日本語…"},
		{"日本語の題名", 6, "日本…"},
		{"日本", 1, "…"},
		{"e\u0301clair", 3, "e\u0301c…"},
		{"👩🏽‍💻abcd", 3, "👩🏽‍💻…"},
		{"👩🏽‍💻abcd", 2, "…"},
		{"🇯🇵abcd", 3, "🇯🇵…"},
		{"a\nbcdef", 5, "a bc…"},
		{"\x1b[31mabcdef\x1b[0m", 4, "abc…"},
	} {
		for _, theme := range []Theme{{}, {ASCII: true}} {
			want := tc.want
			if theme.ASCII && strings.HasSuffix(want, "…") {
				want = want[:len(want)-len("…")] + "~"
			}
			got := tail(tc.text, tc.width, theme)
			if got != want || uniseg.StringWidth(got) > max(0, tc.width) {
				t.Fatalf("tail(%q, %d, %+v) = %q, want %q", tc.text, tc.width, theme, got, want)
			}
		}
	}
	if got := middle("prefix/suffix", 9, Theme{}); got != "pref…ffix" {
		t.Fatalf("path/ID middle truncation changed: %q", got)
	}
}
