// Package terminaltext treats external text as data. Only URLs constructed by
// ParseURL can produce renderer-owned OSC 8; provider escape sequences are lost.
package terminaltext

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/internal/displaytext"
)

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

// ParseURL checks HTTP(S) syntax and removes unsafe terminal targets.
// It never resolves a host or opens a browser.
func ParseURL(value string) (URL, bool) {
	if value == "" || !utf8.ValidString(value) {
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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return URL{}, false
	}
	// URL.String escapes paths/fragments but deliberately leaves RawQuery alone.
	// Escape non-URI bytes there too, preserving query delimiters and valid %XX.
	u.RawQuery = escapeQuery(u.RawQuery)
	target := u.String()
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

func unsafeRune(r rune) bool { return displaytext.UnsafeRune(r) }

func Clean(value string) string { return displaytext.Clean(value) }
