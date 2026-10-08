package terminaltext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestWebURLBoundary(t *testing.T) {
	for _, value := range []string{
		"https://example.com", "http://localhost:8080/a?x=1&y=2#part",
		"https://127.0.0.1/", "http://[::1]:8080/", "HTTPS://example.com/",
		"https://example.com/資料/👩🏽‍💻?q=a%20b", "https://example.com/a_(b)",
		"https://xn--r8jz45g.xn--zckzah/", "https://example.com/a%20b",
		"https://example.com/?q=資料&x=a|b",
	} {
		t.Run(value, func(t *testing.T) {
			u, ok := ParseURL(value)
			if !ok || u.IsZero() || !strings.HasPrefix(u.Open(), "\x1b]8;;http") || !strings.HasSuffix(u.Open(), "\x1b\\") {
				t.Fatal("valid HTTP(S) URL rejected")
			}
			payload := strings.TrimSuffix(strings.TrimPrefix(u.Open(), "\x1b]8;;"), "\x1b\\")
			for _, r := range payload {
				if unsafeRune(r) || unicode.IsSpace(r) {
					t.Fatal("unsafe URL was serialized")
				}
			}
		})
	}
	for _, value := range []string{
		"", "/relative", "//example.com", "file:///tmp/a", "javascript:alert(1)", "ftp://example.com",
		"https:example.com", "https:///missing", "http://", "https://user:password@example.com/",
		"https://user@example.com/", "https://example.com@evil.invalid/", "https://%75ser@example.com/",
		"https://example.com:bad/", "https://example.com:/", "https://example.com:65536/", "https://example.com:0/",
		"https://[not-an-ip]/", "https://::1/", "https://-bad.example/", "https://example..com/",
		"https://example.com\\@evil.invalid/", "https://example.com/a b", "https://example.com/%zz",
		"https://example.com/\x1b]8;;https://evil.invalid\a", "https://example.com/\x1b[2J",
		"https://example.com/%1b%5d52;c;data%07", "https://example.com/%0d%0a", "https://example.com/%5c",
		"https://example.com/\u202e", "https://example.com/%E2%80%AE", "https://example.com/\u2066",
		"https://example.com/\u009c", "https://example.com/\x00", "https://example.com/\xff",
		"https://example.com/<tag>", "https://example.com/?q=\"value\"", "https://example.com/%ff",
		"https://example.com/" + strings.Repeat("a", MaxURLBytes),
	} {
		t.Run(value, func(t *testing.T) {
			if u, ok := ParseURL(value); ok || !u.IsZero() || u.Open() != "" {
				t.Fatal("unsafe/malformed URL accepted")
			}
		})
	}
}

func TestWebURLEscapesWithoutChangingQueryValues(t *testing.T) {
	u, ok := ParseURL("https://example.com/資料?q=資料&x=a|b&encoded=a%20b#章")
	if !ok {
		t.Fatal("valid Unicode URI rejected")
	}
	if u.Open() != "\x1b]8;;https://example.com/%E8%B3%87%E6%96%99?q=%E8%B3%87%E6%96%99&x=a%7Cb&encoded=a%20b#%E7%AB%A0\x1b\\" {
		t.Fatal("path/query/fragment escaping changed")
	}
}

func TestCleanExternalTerminalCommands(t *testing.T) {
	for _, sequence := range []string{
		"\x1b[2J", "\x1b[31m", "\x1b[?25h", "\x1b]52;c;SECRET\a", "\x1b]0;title\x1b\\",
		"\x1b]8;;https://example.com\a", "\x1b]8;;https://example.com\x1b\\", "\x1b]8;;\x1b\\",
		"\x1bPpayload\x1b\\", "\x1b_payload\x1b\\", "\x1bXpayload\x1b\\", "\x1b^payload\x1b\\",
		"\u009b2J", "\u009d52;c;SECRET\u009c", "\u0090SECRET\u009c", "\x1b(B", "\x1b7",
		"\x00\b\a\r", "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c\u2028\u2029",
	} {
		if got := Clean("before" + sequence + "after"); got != "beforeafter" {
			t.Fatalf("terminal command was retained: %q", got)
		}
	}
	text := "猫 👩🏽‍💻 é\n\t資料"
	if Clean(text) != text || Clean("before\x1b]8;;unterminated") != "before" || Clean("before\x1b[31") != "before" {
		t.Fatal("graphemes or incomplete-command handling changed")
	}
}

func FuzzExternalTextNeverExecutes(f *testing.F) {
	for _, value := range []string{"text", "日本語👩‍💻", "\x1b]8;;https://example.com\a", "https://example.com/%1b", "\u009b2J"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		clean := Clean(value)
		if !utf8.ValidString(clean) {
			t.Fatal("invalid UTF-8 after cleaning")
		}
		for _, r := range clean {
			if unsafeRune(r) && r != '\n' && r != '\t' {
				t.Fatal("terminal control survived cleaning")
			}
		}
		if u, ok := ParseURL(value); ok {
			payload := strings.TrimSuffix(strings.TrimPrefix(u.Open(), "\x1b]8;;"), "\x1b\\")
			for _, r := range payload {
				if unsafeRune(r) {
					t.Fatal("control in OSC URL payload")
				}
			}
		}
	})
}
