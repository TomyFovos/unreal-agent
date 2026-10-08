package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

type Key struct {
	Text, Name string
	Paste      bool
}
type Editor struct {
	data   []byte
	cursor int
	Pasted bool
}

func (e *Editor) Clear()       { clear(e.data); e.data = nil; e.cursor = 0; e.Pasted = false }
func (e *Editor) Text() string { return string(e.data) }
func (e *Editor) Display(secret bool) string {
	if secret {
		return "API key> " + strings.Repeat("*", min(64, uniseg.GraphemeClusterCount(string(e.data))))
	}
	return "> " + string(e.data)
}
func (e *Editor) replace(text string) { e.Clear(); e.Apply(Key{Text: text}) }
func (e *Editor) Apply(k Key) {
	boundaries := []int{0}
	g := uniseg.NewGraphemes(string(e.data))
	for g.Next() {
		_, end := g.Positions()
		boundaries = append(boundaries, end)
	}
	previous, next := 0, len(e.data)
	for _, b := range boundaries {
		if b < e.cursor {
			previous = b
		}
		if b > e.cursor {
			next = b
			break
		}
	}
	switch k.Name {
	case "left":
		e.cursor = previous
	case "right":
		e.cursor = next
	case "home":
		e.cursor = 0
	case "end":
		e.cursor = len(e.data)
	case "backspace":
		if e.cursor > 0 {
			copy(e.data[previous:], e.data[e.cursor:])
			n := len(e.data) - (e.cursor - previous)
			clear(e.data[n:])
			e.data = e.data[:n]
			e.cursor = previous
		}
	default:
		text := SafeText(k.Text)
		if len(e.data)+len(text) > 65536 {
			return
		}
		e.Pasted = e.Pasted || k.Paste
		old := len(e.data)
		e.data = append(e.data, make([]byte, len(text))...)
		copy(e.data[e.cursor+len(text):], e.data[e.cursor:old])
		copy(e.data[e.cursor:], text)
		e.cursor += len(text)
	}
}

// Decoder recognizes bounded ANSI key/paste sequences across arbitrary reads.
type Decoder struct {
	pending []byte
	paste   bool
}

// FlushEscape is called only after an idle read deadline. Split CSI/paste
// sequences remain buffered; a lone nonpasted Escape cancels a UI picker.
func (d *Decoder) FlushEscape() []Key {
	if !d.paste && len(d.pending) == 1 && d.pending[0] == 27 {
		d.pending = nil
		return []Key{{Name: "escape"}}
	}
	return nil
}

func (d *Decoder) Feed(data []byte) []Key {
	d.pending = append(d.pending, data...)
	var keys []Key
	for len(d.pending) > 0 {
		b := d.pending[0]
		if b == 27 {
			if len(d.pending) < 2 {
				break
			}
			if d.pending[1] != '[' {
				d.pending = d.pending[2:]
				continue
			}
			end := 2
			for end < len(d.pending) && end < 20 && (d.pending[end] < 0x40 || d.pending[end] > 0x7e) {
				end++
			}
			if end == len(d.pending) && end < 20 {
				break
			}
			if end >= 20 {
				d.pending = d.pending[1:]
				continue
			}
			sequence := string(d.pending[:end+1])
			d.pending = d.pending[end+1:]
			if sequence == "\x1b[200~" {
				d.paste = true
				continue
			}
			if sequence == "\x1b[201~" {
				d.paste = false
				continue
			}
			if !d.paste {
				if name := map[string]string{"\x1b[A": "up", "\x1b[B": "down", "\x1b[D": "left", "\x1b[C": "right", "\x1b[H": "home", "\x1b[F": "end", "\x1b[5~": "pageup", "\x1b[6~": "pagedown"}[sequence]; name != "" {
					keys = append(keys, Key{Name: name})
				}
			}
			continue
		}
		if !utf8.FullRune(d.pending) {
			break
		}
		r, n := utf8.DecodeRune(d.pending)
		d.pending = d.pending[n:]
		if d.paste {
			keys = append(keys, Key{Text: string(r), Paste: true})
			continue
		}
		name := map[rune]string{3: "cancel", 4: "detach", 9: "tab", 13: "enter", 10: "enter", 127: "backspace", 8: "backspace"}[r]
		if name != "" {
			keys = append(keys, Key{Name: name})
		} else if r >= 32 {
			keys = append(keys, Key{Text: string(r)})
		}
	}
	return keys
}
