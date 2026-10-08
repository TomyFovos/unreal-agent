// Package terminaltext treats external text as data. Only URLs constructed by
// ParseURL can produce renderer-owned OSC 8; provider escape sequences are lost.
package terminaltext

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxURLBytes = 4096

// URL has no public writable target: its zero value means no hyperlink.
type URL struct{ target string }

func (u URL) Open() string {
	if u.target == "" {
		return ""
	}
	return "\x1b]8;;" + u.target + "\x1b\\"
}

func (u URL) IsZero() bool { return u.target == "" }

const Close = "\x1b]8;;\x1b\\"

// ParseURL extends upstream terminaltext's HTTP(S)/hostname check with a closed
// authority boundary. It never resolves a host or opens a browser.
func ParseURL(value string) (URL, bool) {
	if value == "" || len(value) > MaxURLBytes || !utf8.ValidString(value) {
		return URL{}, false
	}
	for _, r := range value {
		if unsafeRune(r) || unicode.IsSpace(r) || strings.ContainsRune("\\<>\"", r) || r == utf8.RuneError {
			return URL{}, false
		}
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return URL{}, false
	}
	for _, r := range decoded {
		if unsafeRune(r) || r == '\\' || r == utf8.RuneError {
			return URL{}, false
		}
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || !validHost(u.Hostname()) {
		return URL{}, false
	}
	if strings.HasPrefix(u.Host, "[") && net.ParseIP(u.Hostname()) == nil || strings.Contains(u.Hostname(), ":") && !strings.HasPrefix(u.Host, "[") {
		return URL{}, false
	}
	if strings.HasSuffix(u.Host, ":") {
		return URL{}, false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return URL{}, false
		}
	}
	// URL.String escapes paths/fragments but deliberately leaves RawQuery alone.
	// Escape non-URI bytes there too, preserving query delimiters and valid %XX.
	u.RawQuery = escapeQuery(u.RawQuery)
	target := u.String()
	if len(target) > MaxURLBytes {
		return URL{}, false
	}
	return URL{target: target}, true
}

func escapeQuery(value string) string {
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~!$&'()*+,;=:@/?%"
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if strings.IndexByte(allowed, b) >= 0 {
			out.WriteByte(b)
		} else {
			out.WriteByte('%')
			out.WriteByte(hex[b>>4])
			out.WriteByte(hex[b&15])
		}
	}
	return out.String()
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func unsafeRune(r rune) bool {
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
			if r == '\n' || r == '\t' || !unsafeRune(r) {
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
