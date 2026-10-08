package tui

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui/terminaltext"
)

var webURLPattern = regexp.MustCompile("(?i)https?://[^\\s<>\"`]+")

const maxBareLinksPerLine = 256

// webTextLine only annotates already visible text. Paths remain visible in
// plain/unsupported terminals, and insertion/selection never performs an action.
func webTextLine(text string, s style) line {
	l := textLine(text, s)
	if terminaltext.Clean(text) != text || !strings.Contains(strings.ToLower(text), "http") {
		return l
	}
	text = strings.ReplaceAll(text, "\t", "    ")
	glyphIndex, position := 0, 0
	for _, match := range webURLPattern.FindAllStringIndex(text, maxBareLinksPerLine) {
		start, end := match[0], match[1]
		if start > 0 {
			if !webBoundary(text[:start]) {
				continue
			}
		}
		for end > start {
			r, n := utf8.DecodeLastRuneInString(text[start:end])
			if strings.ContainsRune(".,!?;。、『』」", r) ||
				r == ')' && strings.Count(text[start:end], ")") > strings.Count(text[start:end], "(") ||
				r == ']' && strings.Count(text[start:end], "]") > strings.Count(text[start:end], "[") {
				end -= n
			} else {
				break
			}
		}
		target, ok := terminaltext.ParseURL(text[start:end])
		if !ok {
			continue
		}
		for glyphIndex < len(l) && position < start {
			position += len(l[glyphIndex].text)
			glyphIndex++
		}
		for glyphIndex < len(l) && position < end {
			next := position + len(l[glyphIndex].text)
			if next <= end {
				l[glyphIndex].link = target
			}
			position = next
			glyphIndex++
		}
	}
	return l
}

func webBoundary(prefix string) bool {
	r, _ := utf8.DecodeLastRuneInString(prefix)
	return unicode.IsSpace(r) || strings.ContainsRune("([{\"'", r)
}

func clearLinks(l line) {
	for i := range l {
		l[i].link = terminaltext.URL{}
	}
}

func linkedText(text string, s style, target terminaltext.URL) line {
	l := textLine(text, s)
	for i := range l {
		l[i].link = target
	}
	return l
}

// Find the closing delimiter without truncating parentheses inside a URL.
func linkEnd(s string) int {
	depth := 1
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
