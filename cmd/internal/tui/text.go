package tui

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui/terminaltext"
)

type glyph struct {
	text  string
	width int
	style style
	link  terminaltext.URL
}
type line []glyph

func textLine(text string, s style) line {
	var result line
	g := uniseg.NewGraphemes(strings.ReplaceAll(terminaltext.Clean(text), "\t", "    "))
	for g.Next() {
		result = append(result, glyph{text: g.Str(), width: g.Width(), style: s})
	}
	return result
}
func (l line) width() int {
	n := 0
	for _, g := range l {
		n += g.width
	}
	return n
}
func (l line) plain() string {
	var b strings.Builder
	for _, g := range l {
		b.WriteString(g.text)
	}
	return b.String()
}
func (l line) clip(width int) line {
	end, n := 0, 0
	for i, g := range l {
		if n+g.width > width {
			break
		}
		n += g.width
		end = i + 1
	}
	return l[:end]
}
func (l line) paint(t Theme) string {
	var b strings.Builder
	current := normal
	var link terminaltext.URL
	for _, g := range l {
		if t.Hyperlinks && !t.Plain && g.link != link {
			if !link.IsZero() {
				b.WriteString(terminaltext.Close)
			}
			b.WriteString(g.link.Open())
			link = g.link
		}
		if g.style != current && !t.Plain {
			if current != normal {
				b.WriteString("\x1b[0m")
			}
			b.WriteString(t.sgr(g.style))
			current = g.style
		}
		b.WriteString(g.text)
	}
	if !link.IsZero() {
		b.WriteString(terminaltext.Close)
	}
	if current != normal && !t.Plain {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
func joined(parts ...line) line {
	var l line
	for _, p := range parts {
		l = append(l, p...)
	}
	return l
}

func middle(text string, width int, t Theme) string {
	l := textLine(strings.ReplaceAll(SafeText(text), "\n", " "), normal)
	if l.width() <= width {
		return l.plain()
	}
	if width < 2 {
		return l.clip(max(0, width)).plain()
	}
	mark := t.symbol("…", "~")
	left := l.clip((width - 1) / 2)
	n := width - 1 - left.width()
	start := len(l)
	for start > 0 && l[start-1].width <= n {
		start--
		n -= l[start].width
	}
	return left.plain() + mark + line(l[start:]).plain()
}

const noStart = "、。，．）」』】〉》！？ー"
const noEnd = "（「『【〈《"

func forbidden(g glyph, chars string) bool { return strings.ContainsAny(g.text, chars) }
func latin(g glyph) bool {
	for _, r := range g.text {
		return r < 0x2e80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/_-.:", r))
	}
	return false
}

// wrapLine works in grapheme cells, with Latin word boundaries and CJK kinsoku.
// Code uses hard wrapping, preserving indentation on every continuation.
func wrapLine(l line, width int, hard bool, indent int) []line {
	width = max(1, width)
	if len(l) == 0 {
		return []line{nil}
	}
	var out []line
	first := true
	for len(l) > 0 {
		padding := 0
		if !first && indent > 0 {
			padding = min(indent, width-1)
		}
		room := width - padding
		n, cells := 0, 0
		for n < len(l) && cells+l[n].width <= room {
			cells += l[n].width
			n++
		}
		if n == 0 {
			out = append(out, textLine("�", normal))
			l = l[1:]
			first = false
			continue
		}
		if !hard && n < len(l) {
			if latin(l[n-1]) && latin(l[n]) {
				for i := n - 1; i > 0; i-- {
					if l[i].text == " " {
						n = i
						break
					}
				}
			}
			for n > 1 && (forbidden(l[n], noStart) || forbidden(l[n-1], noEnd)) {
				n--
			}
		}
		part := append(line(nil), l[:n]...)
		if !hard && n < len(l) {
			for len(part) > 0 && part[len(part)-1].text == " " {
				part = part[:len(part)-1]
			}
		}
		if padding > 0 {
			part = joined(textLine(strings.Repeat(" ", padding), normal), part)
		}
		out = append(out, part)
		l = l[n:]
		if !hard {
			for len(l) > 0 && l[0].text == " " {
				l = l[1:]
			}
		}
		first = false
	}
	return out
}

var bulletRE = regexp.MustCompile(`^(?:[-*] |[0-9]+\. )`)

func markdown(s string) line {
	// Inline code is left verbatim; unsupported Markdown (including tables) is data.
	clean := terminaltext.Clean(s)
	unsafeLinks := clean != s
	s = clean
	var out line
	bareLinks := 0
	for len(s) > 0 {
		if s[0] == '`' {
			if end := strings.Index(s[1:], "`"); end >= 0 {
				end += 2
				out = append(out, textLine(s[:end], normal)...)
				s = s[end:]
				continue
			}
		}
		if strings.HasPrefix(s, "**") {
			if end := strings.Index(s[2:], "**"); end >= 0 {
				out = append(out, textLine(s[2:2+end], strong)...)
				s = s[4+end:]
				continue
			}
		}
		if s[0] == '[' {
			if end := strings.Index(s, "]("); end > 0 {
				if close := linkEnd(s[end+2:]); close >= 0 {
					target, _ := terminaltext.ParseURL(s[end+2 : end+2+close])
					out = append(out, linkedText(s[1:end], normal, target)...)
					out = append(out, textLine(" (", normal)...)
					out = append(out, linkedText(s[end+2:end+2+close], meta, target)...)
					out = append(out, textLine(")", normal)...)
					s = s[end+3+close:]
					continue
				}
			}
		}
		if len(s) >= 7 && strings.EqualFold(s[:7], "http://") || len(s) >= 8 && strings.EqualFold(s[:8], "https://") {
			if match := webURLPattern.FindStringIndex(s); match != nil && match[0] == 0 {
				part := textLine(s[:match[1]], normal)
				if bareLinks < maxBareLinksPerLine {
					part = webTextLine(s[:match[1]], normal)
				}
				bareLinks++
				if len(out) > 0 && !webBoundary(out[len(out)-1].text) {
					clearLinks(part)
				}
				out = append(out, part...)
				s = s[match[1]:]
				continue
			}
		}
		g := uniseg.NewGraphemes(s)
		g.Next()
		out = append(out, textLine(g.Str(), normal)...)
		_, n := g.Positions()
		s = s[n:]
	}
	if unsafeLinks {
		clearLinks(out)
	}
	return out
}

type bodyLine struct {
	text line
	seam string
	role string
}

func entryBody(e Entry, width int, t Theme) []bodyLine {
	var result []bodyLine
	code := false
	text := e.Text
	if e.PeerID != "" {
		text = e.PeerID + " [" + e.PeerKind + "] " + text
	}
	if e.Role == "host" {
		text = strings.Join(strings.Fields(text), " ")
	}
	unsafeLinks := e.UnsafeLinks || terminaltext.Clean(text) != text
	text = terminaltext.Clean(text)
	for _, raw := range strings.Split(text, "\n") {
		l := webTextLine(raw, normal)
		hard, indent := false, 0
		if e.Role == "host" {
			l = textLine(raw, meta)
		}
		if e.Role == "agent" {
			if strings.HasPrefix(strings.TrimSpace(raw), "```") || strings.HasPrefix(strings.TrimSpace(raw), "~~~") {
				l = textLine(raw, meta)
				code = !code
			} else if code {
				hard = true
				leading := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]
				indent = len(strings.ReplaceAll(leading, "\t", "    "))
			} else {
				l = markdown(raw)
				if strings.HasPrefix(strings.TrimSpace(raw), "|") {
					l = textLine(raw, normal)
				}
				for _, prefix := range []string{"### ", "## ", "# "} {
					if strings.HasPrefix(raw, prefix) {
						l = textLine(strings.TrimPrefix(raw, prefix), strong)
						break
					}
				}
				if strings.HasPrefix(raw, ">") && len(l) > 0 {
					l[0].style = meta
				}
				indent = len(bulletRE.FindString(raw))
			}
		}
		if unsafeLinks || code || hard {
			clearLinks(l)
		}
		if e.PeerID != "" && len(result) == 0 {
			for i := range l {
				if i < len(textLine(e.PeerID, strong)) {
					l[i].style = strong
				} else if i < len(textLine(e.PeerID+" ["+e.PeerKind+"]", meta)) {
					l[i].style = meta
				}
			}
		}
		wrapped := wrapLine(l, width, hard, indent)
		for i, p := range wrapped {
			seam := ""
			if i > 0 && hard {
				seam = t.symbol("↪", ">")
			}
			result = append(result, bodyLine{text: p, seam: seam})
		}
	}
	if e.Clipped {
		for _, p := range wrapLine(textLine(clippedMessage, meta), width, false, 0) {
			result = append(result, bodyLine{text: p})
		}
	}
	if hint := failureHint(e.Code); hint != "" {
		for _, p := range wrapLine(textLine(hint, meta), width, false, 0) {
			result = append(result, bodyLine{text: p})
		}
	}
	return result
}
func failureHint(code string) string {
	switch code {
	case "external_reauth_required":
		return "Renew the login with Codex CLI (codex login) outside this TUI; the next request rereads the file."
	case "writer_owned":
		return "Another client owns the writer; /resume cannot acquire it until that owner releases it."
	}
	return ""
}
