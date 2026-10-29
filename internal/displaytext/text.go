// Package displaytext shares canonical display text and human-facing markers
// between Host metadata and terminal projections.
package displaytext

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

const MessageBytes = 64 << 10

func UnsafeRune(r rune) bool {
	return unicode.IsControl(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 ||
		r == 0x200e || r == 0x200f || r == 0x061c || r == 0x2028 || r == 0x2029
}

// Clean follows upstream Clean's removal of complete terminal commands, without
// importing its TUI/ANSI framework or trusting its CleanStyled raw-OSC path.
// Newlines, tabs and grapheme joining characters retain the Fork's semantics.
func Clean(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		r, n := utf8.DecodeRuneInString(value[i:])
		i += n
		switch r {
		case '\x1b':
			if i == len(value) {
				continue
			}
			next := value[i]
			i++
			switch next {
			case '[':
				i = skipCSI(value, i)
			case ']', 'P', 'X', '^', '_':
				i = skipString(value, i)
			default:
				for next >= 0x20 && next <= 0x2f && i < len(value) {
					next = value[i]
					i++
				}
			}
		case '\u009b':
			i = skipCSI(value, i)
		case '\u009d', '\u0090', '\u0098', '\u009e', '\u009f':
			i = skipString(value, i)
		default:
			if r == '\n' || r == '\t' || !UnsafeRune(r) {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func skipCSI(value string, i int) int {
	for i < len(value) {
		b := value[i]
		i++
		if b >= 0x40 && b <= 0x7e {
			break
		}
	}
	return i
}

func skipString(value string, i int) int {
	for i < len(value) {
		if strings.HasPrefix(value[i:], "\x1b\\") {
			return i + 2
		}
		r, n := utf8.DecodeRuneInString(value[i:])
		i += n
		if r == '\a' || r == '\u009c' {
			break
		}
	}
	return i
}

// Message preserves the existing terminal's per-message display boundary.
func Message(value string) string {
	value = Clean(value)
	if len(value) <= MessageBytes {
		return value
	}
	end := 0
	g := uniseg.NewGraphemes(value)
	for g.Next() {
		_, next := g.Positions()
		if next > MessageBytes {
			break
		}
		end = next
	}
	return value[:end]
}
func ForHuman(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "【報告】") || strings.HasPrefix(text, "【要確認】")
}
func Attention(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "【要確認】") {
		return "", false
	}
	text = strings.TrimSpace(strings.TrimPrefix(text, "【要確認】"))
	first, _, _ := strings.Cut(text, "\n")
	return first, true
}
