package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

type Key struct {
	Text, Name string
	Paste      bool
	// Mouse positions are one-based SGR terminal cells, never composer text.
	MouseX, MouseY int
}
type Editor struct {
	data     []byte
	cursor   int
	Pasted   bool
	Selected bool
	// pastes holds long pasted texts; the composer shows a short marker for
	// each, as Claude Code and Codex do, and Expanded restores them.
	pastes []string
}

func (e *Editor) Clear() {
	clear(e.data)
	e.data, e.cursor, e.Pasted, e.pastes, e.Selected = nil, 0, false, nil, false
}
func (e *Editor) Text() string { return string(e.data) }

// continuesLine reports a backslash just before the cursor at the end of the
// text: Enter then starts a new line, as in Claude Code.
func (e *Editor) continuesLine() bool {
	return e.cursor == len(e.data) && len(e.data) > 0 && e.data[len(e.data)-1] == '\\'
}

// Expanded is the text to submit: each paste marker still in the composer
// becomes the text it stands for.
func (e *Editor) Expanded() string {
	text := string(e.data)
	for i, p := range e.pastes {
		text = strings.Replace(text, pasteMarker(i+1, p), p, 1)
	}
	return text
}

// A paste of several lines or about a screen of text is shown as a marker.
func longPaste(text string) bool {
	return strings.Count(text, "\n") >= 2 || len(text) > 800
}

func pasteMarker(n int, text string) string {
	lines := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1
	return "[Pasted text #" + strconv.Itoa(n) + " +" + strconv.Itoa(lines) + " lines]"
}
func (e *Editor) Display(secret bool) string {
	if secret {
		return "API key> " + strings.Repeat("*", min(64, uniseg.GraphemeClusterCount(string(e.data))))
	}
	return "> " + string(e.data)
}
func (e *Editor) replace(text string) { e.Clear(); e.Apply(Key{Text: text}) }
func (e *Editor) Apply(k Key) {
	if k.Name == "selectall" && !k.Paste {
		e.Selected = len(e.data) > 0
		return
	}
	if e.Selected {
		switch k.Name {
		case "backspace", "delete":
			e.Clear()
			return
		case "left", "right", "up", "down", "home", "end", "escape", "historyhome", "historyend", "pageup", "pagedown":
			e.Selected = false
		default:
			if k.Text != "" || k.Paste {
				e.Clear()
			}
		}
	}
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
	case "delete":
		if e.cursor < len(e.data) {
			copy(e.data[e.cursor:], e.data[next:])
			n := len(e.data) - (next - e.cursor)
			clear(e.data[n:])
			e.data = e.data[:n]
		}
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
		if k.Paste && longPaste(text) {
			e.pastes = append(e.pastes, text)
			text = pasteMarker(len(e.pastes), text)
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
	// pasted collects one bracketed paste, which becomes a single key: one
	// key per character made a long paste slow to appear.
	pasted strings.Builder
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
			if d.pending[1] == 'O' { // xterm application-mode Home/End
				if len(d.pending) < 3 {
					break
				}
				if !d.paste {
					if name := map[byte]string{'H': "home", 'F': "end"}[d.pending[2]]; name != "" {
						keys = append(keys, Key{Name: name})
					}
				}
				d.pending = d.pending[3:]
				continue
			}
			if d.pending[1] != '[' {
				if (d.pending[1] == '\r' || d.pending[1] == '\n') && !d.paste {
					keys = append(keys, Key{Name: "newline", Text: "\n"}) // Alt+Enter
				}
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
				if d.pasted.Len() > 0 {
					keys = append(keys, Key{Text: d.pasted.String(), Paste: true})
					d.pasted.Reset()
				}
				continue
			}
			if !d.paste {
				if key, ok := mouseWheel(sequence); ok {
					keys = append(keys, key)
				} else if newlineSequence(sequence) {
					keys = append(keys, Key{Name: "newline", Text: "\n"})
				} else if name := map[string]string{"\x1b[3~": "delete", "\x1b[A": "up", "\x1b[B": "down", "\x1b[D": "left", "\x1b[C": "right", "\x1b[H": "home", "\x1b[F": "end", "\x1b[1~": "home", "\x1b[4~": "end", "\x1b[7~": "home", "\x1b[8~": "end", "\x1b[5~": "pageup", "\x1b[6~": "pagedown", "\x1b[1;5H": "historyhome", "\x1b[1;5F": "historyend", "\x1b[7;5~": "historyhome", "\x1b[8;5~": "historyend"}[sequence]; name != "" {
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
			d.pasted.WriteRune(r)
			continue
		}
		name := map[rune]string{1: "selectall", 3: "cancel", 4: "detach", 9: "tab", 13: "enter", 127: "backspace", 8: "backspace"}[r]
		if r == 10 { // Ctrl+J, and Ctrl+Enter in many terminals
			keys = append(keys, Key{Name: "newline", Text: "\n"})
		} else if name != "" {
			keys = append(keys, Key{Name: name})
		} else if r >= 32 {
			keys = append(keys, Key{Text: string(r)})
		}
	}
	return keys
}

// newlineSequence is Enter with Ctrl, Shift or Alt in the kitty keyboard
// protocol (CSI 13;mod u) or xterm's modifyOtherKeys (CSI 27;mod;13 ~).
func newlineSequence(sequence string) bool {
	for _, mod := range []string{"2", "3", "5", "6"} {
		if sequence == "\x1b[13;"+mod+"u" || sequence == "\x1b[27;"+mod+";13~" {
			return true
		}
	}
	return false
}

// Only wheel presses have meaning. Mouse clicks/releases/motion, malformed
// parameters and pasted reports cannot select, approve, submit or insert text.
func mouseWheel(sequence string) (Key, bool) {
	if !strings.HasPrefix(sequence, "\x1b[<") || !strings.HasSuffix(sequence, "M") {
		return Key{}, false
	}
	params := strings.Split(sequence[3:len(sequence)-1], ";")
	if len(params) != 3 {
		return Key{}, false
	}
	var values [3]int
	for i, part := range params {
		if len(part) == 0 || len(part) > 4 {
			return Key{}, false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return Key{}, false
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return Key{}, false
		}
		values[i] = n
	}
	button := values[0]
	if button & ^(64|1|4|8|16) != 0 || button&64 == 0 || values[1] < 1 || values[2] < 1 {
		return Key{}, false
	}
	name := "wheelup"
	if button&1 != 0 {
		name = "wheeldown"
	}
	return Key{Name: name, MouseX: values[1], MouseY: values[2]}, true
}
