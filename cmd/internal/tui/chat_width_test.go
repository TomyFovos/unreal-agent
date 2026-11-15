package tui

import (
	"regexp"
	"strings"
	"testing"
)

var loremRun = regexp.MustCompile(`lorem( lorem)*`)

// The Chat view shows only the conversation, so its text uses the whole
// terminal width; Split keeps the reading width beside the work pane.
func TestChatViewUsesTheWholeWidth(t *testing.T) {
	widest := func(view ViewMode, width int) int {
		s, u := orchestrationFixture()
		s.Progress = nil
		s.Entries = []Entry{{Role: "agent", Text: strings.Repeat("lorem ", 120)}}
		u.View = view
		f := RenderFrame(s, u, width, 48)
		most := 0
		for _, row := range f.Lines {
			for _, run := range loremRun.FindAllString(row, -1) {
				most = max(most, len(run))
			}
		}
		return most
	}
	if got := widest(ViewChat, 200); got < 180 {
		t.Fatalf("Chat at 200 columns wraps at %d columns", got)
	}
	if got := widest(ViewChat, 80); got > 80 {
		t.Fatalf("Chat at 80 columns overflows to %d columns", got)
	}
	if got := widest(ViewSplit, 200); got > 110 {
		t.Fatalf("Split at 200 columns lost its reading width: %d columns", got)
	}
}
