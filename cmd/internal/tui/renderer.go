package tui

import (
	"fmt"
	"strings"
)

// Renderer replaces changed rows in place. It never clears the whole screen,
// writes the right margin, or inserts a printable substitute for the cursor.
type Renderer struct {
	previous      []string
	width, height int
}

func (r *Renderer) Draw(f Frame) string {
	if f.Height == 0 {
		return ""
	}
	var b strings.Builder
	resize := r.width != f.Width || r.height != f.Height
	for i, text := range f.Lines {
		if !resize && i < len(r.previous) && r.previous[i] == text {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[K", i+1, text)
	}
	for i := len(f.Lines); i < min(len(r.previous), f.Height); i++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[K", i+1)
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", max(1, f.CursorY+1), max(1, f.CursorX+1))
	r.previous = append(r.previous[:0], f.Lines...)
	r.width = f.Width
	r.height = f.Height
	return b.String()
}
