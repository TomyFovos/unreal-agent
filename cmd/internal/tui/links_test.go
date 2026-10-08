package tui

import (
	"encoding/json/v2"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui/terminaltext"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

var linkOSC = regexp.MustCompile(`\x1b\]8;;([^\x1b]*)\x1b\\`)

func visibleLinkText(s string) string { return terminaltext.Clean(s) }

func TestHyperlinkMarkdownBareURLsAndUnicode(t *testing.T) {
	for _, input := range []string{
		"[資料👩🏽‍💻é](https://example.com/資料/👩🏽‍💻)",
		"see https://example.com/a_(b). next", "\thttp://localhost:8080/?a=1&b=2",
	} {
		rows := entryBody(Entry{Role: "agent", Text: input}, 200, Theme{})
		var painted, plain strings.Builder
		for _, row := range rows {
			painted.WriteString(row.text.paint(Theme{Hyperlinks: true}))
			plain.WriteString(row.text.plain())
		}
		if !strings.Contains(painted.String(), "\x1b]8;;http") || visibleLinkText(painted.String()) != plain.String() {
			t.Fatal("hyperlinks changed the visible text or were missing")
		}
		assertOwnedLinks(t, painted.String())
	}
	for _, input := range []string{"[docs](url)", "[local](file:///tmp/a)", "[auth](https://u:p@example.com)", "`https://example.com`", "foohttps://example.com"} {
		for _, row := range entryBody(Entry{Role: "agent", Text: input}, 200, Theme{}) {
			if strings.Contains(row.text.paint(Theme{Hyperlinks: true}), "\x1b]8;") {
				t.Fatal("invalid URL, inline code or embedded word became clickable")
			}
		}
	}
}

func assertOwnedLinks(t *testing.T, painted string) {
	t.Helper()
	active := false
	for _, match := range linkOSC.FindAllStringSubmatch(painted, -1) {
		if match[1] == "" {
			if !active {
				t.Fatal("unmatched hyperlink close")
			}
			active = false
		} else {
			if _, ok := terminaltext.ParseURL(match[1]); !ok || active {
				t.Fatal("unsafe URL or nested hyperlink")
			}
			active = true
		}
	}
	if active {
		t.Fatal("hyperlink leaked beyond its row")
	}
	// The only control sequences are the renderer's closed SGR set and OSC 8.
	rest := linkOSC.ReplaceAllString(painted, "")
	rest = sgrRE.ReplaceAllString(rest, "")
	if strings.ContainsAny(rest, "\x1b\a\u009b\u009d\u202e\u2066") {
		t.Fatal("provider terminal control escaped into rendering")
	}
}

func TestHyperlinkInjectionStaysPlainThroughCanonicalProjection(t *testing.T) {
	for _, input := range []string{
		"[docs](https://example.com/\x1b]8;;https://evil.invalid\a)",
		"\x1b[31m[docs](https://example.com)\x1b[0m",
		"\x1b]8;;https://evil.invalid\x1b\\[docs](https://example.com)\x1b]8;;\x1b\\",
		"[docs](https://example.com/\u202e)", "[docs](https://example.com/\u2066)",
		"[docs](https://example.com/%1b%5d8;evil)", "[docs](https://user@example.com)",
		"[\x1b]52;c;SECRET\a docs](https://example.com)",
	} {
		record := host.HistoryItem{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: input}}}}}}
		before, _ := json.Marshal(record)
		for _, entry := range append(entriesFor(record), Entry{Role: "agent", Text: input}) {
			for _, row := range entryBody(entry, 100, Theme{}) {
				painted := row.text.paint(Theme{Hyperlinks: true})
				if strings.Contains(painted, "\x1b]8;") {
					t.Fatal("sanitization made an unsafe provider link clickable")
				}
				assertOwnedLinks(t, painted)
			}
		}
		after, _ := json.Marshal(record)
		if string(before) != string(after) {
			t.Fatal("projection mutated canonical history")
		}
	}
}

func TestHyperlinkGraphemeWrappingClippingAndRowBoundaries(t *testing.T) {
	l := markdown("[猫👨‍👩‍👧‍👦é👩🏽‍💻](https://example.com/資料?q=a%20b) tail")
	for _, width := range []int{1, 2, 3, 7, 20, 120} {
		for _, row := range wrapLine(l, width, false, 0) {
			painted := row.clip(width).paint(Theme{Hyperlinks: true})
			assertOwnedLinks(t, painted)
			if visibleLinkText(painted) != row.clip(width).plain() || uniseg.StringWidth(visibleLinkText(painted)) > width {
				t.Fatal("OSC bytes changed width or grapheme wrapping")
			}
		}
	}
	if got := l.clip(0).paint(Theme{Hyperlinks: true}); got != "" {
		t.Fatal("zero-width clip emitted a hyperlink")
	}
}

func TestHyperlinkResponsiveSplitResizeCacheAndExport(t *testing.T) {
	s, u := idleSnapshot(), childUI(2)
	u.cache = &bodyCache{}
	u.Input = "https://composer.invalid/猫é"
	u.Cursor = len(u.Input)
	s.Entries = []Entry{{Role: "agent", Text: "[資料👩🏽‍💻é](https://example.com/資料)\nhttps://example.com/a_(b)"}}
	for _, theme := range []Theme{{Hyperlinks: true}, {Hyperlinks: true, NoColor: true}, {Hyperlinks: true, ASCII: true}, {Hyperlinks: true, Plain: true}, {}} {
		u.Theme = theme
		for _, view := range []ViewMode{ViewChat, ViewSplit, ViewOrchestration} {
			u.View = view
			for _, size := range [][2]int{{1, 1}, {19, 5}, {44, 14}, {60, 20}, {80, 24}, {120, 40}, {160, 40}, {80, 24}} {
				f := RenderFrame(s, u, size[0], size[1])
				plain := f
				plain.Lines = append([]string(nil), f.Lines...)
				for i, row := range f.Lines {
					assertOwnedLinks(t, row)
					plain.Lines[i] = visibleLinkText(row)
					if theme.Plain || !theme.Hyperlinks {
						if strings.Contains(row, "\x1b]8;") {
							t.Fatal("unsupported/plain fallback emitted OSC")
						}
					}
					if theme.NoColor {
						for _, match := range sgrRE.FindAllStringSubmatch(row, -1) {
							for _, code := range strings.Split(match[1], ";") {
								if code != "0" && code != "1" && code != "2" && code != "7" {
									t.Fatal("NO_COLOR emitted a foreground color")
								}
							}
						}
					}
				}
				bounds(t, plain, size[0], size[1])
				if strings.Contains(f.Lines[len(f.Lines)-1], "\x1b]8;") {
					t.Fatal("composer acquired clickable links")
				}
			}
		}
	}
	fake := conversationFixture(s.Entries[0].Text)
	before, _ := json.Marshal(fake.view.History.Items)
	path, err := ExportLastResponse(t.Context(), fake, "s", throughHistory(fake), filepath.Join(t.TempDir(), "exports"))
	if err != nil || exportBody(t, path) != s.Entries[0].Text {
		t.Fatal("TUI hyperlinks changed canonical Markdown export", err)
	}
	after, _ := json.Marshal(fake.view.History.Items)
	if string(before) != string(after) || len(fake.inputs) != 0 || len(fake.view.Operations) != 0 {
		t.Fatal("viewing/exporting hyperlinks submitted work")
	}
}

func TestHyperlinkSanitizationFlagInvalidatesCachedBody(t *testing.T) {
	c := &bodyCache{}
	e := Entry{Role: "agent", Text: "[docs](https://example.com)"}
	c.begin(100, false)
	if !strings.Contains(c.get(e, 100, Theme{})[0].text.paint(Theme{Hyperlinks: true}), "\x1b]8;") {
		t.Fatal("normal entry missing link")
	}
	e.UnsafeLinks = true
	if strings.Contains(c.get(e, 100, Theme{})[0].text.paint(Theme{Hyperlinks: true}), "\x1b]8;") {
		t.Fatal("cached safe entry bypassed provider sanitization")
	}
}

func TestHyperlinkCandidateLimitFallsBackWithoutTruncatingText(t *testing.T) {
	text := strings.Repeat("https://example.com ", maxBareLinksPerLine+20)
	for _, l := range []line{webTextLine(text, normal), markdown(text)} {
		if l.plain() != text {
			t.Fatal("candidate bound truncated conversation text")
		}
		count := 0
		for _, match := range linkOSC.FindAllStringSubmatch(l.paint(Theme{Hyperlinks: true}), -1) {
			if match[1] != "" {
				count++
			}
		}
		if count != maxBareLinksPerLine {
			t.Fatal("bare link detection exceeded its per-line budget")
		}
	}
}

func TestHyperlinkTerminalCapabilityFallback(t *testing.T) {
	for _, fixture := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"unknown xterm", map[string]string{"TERM": "xterm-256color"}, false},
		{"dumb", map[string]string{"TERM": "dumb", "TERM_PROGRAM": "WezTerm"}, false},
		{"missing TERM", map[string]string{"TERM_PROGRAM": "vscode"}, false},
		{"plain", map[string]string{"TERM": "xterm", "TERM_PROGRAM": "vscode", "UNREAL_AGENT_TUI_PLAIN": "1"}, false},
		{"tmux", map[string]string{"TERM": "tmux-256color", "TERM_PROGRAM": "WezTerm"}, false},
		{"inherited multiplexer", map[string]string{"TERM": "xterm", "TERM_PROGRAM": "vscode", "TMUX": "present"}, false},
		{"screen", map[string]string{"TERM": "screen", "TERM_PROGRAM": "vscode"}, false},
		{"vscode", map[string]string{"TERM": "xterm-256color", "TERM_PROGRAM": "vscode"}, true},
		{"kitty", map[string]string{"TERM": "xterm-kitty"}, true},
		{"foot", map[string]string{"TERM": "foot"}, true},
		{"Windows terminal", map[string]string{"TERM": "xterm", "WT_SESSION": "present"}, true},
		{"VTE", map[string]string{"TERM": "xterm", "VTE_VERSION": "5000"}, true},
		{"old VTE", map[string]string{"TERM": "xterm", "VTE_VERSION": "4999"}, false},
		{"NO_COLOR ASCII", map[string]string{"TERM": "xterm", "TERM_PROGRAM": "WezTerm", "NO_COLOR": "1", "UNREAL_AGENT_TUI_ASCII": "1"}, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			theme := EnvironmentTheme(func(name string) string { return fixture.env[name] })
			if theme.Hyperlinks != fixture.want {
				t.Fatal("unsafe or missing terminal capability decision")
			}
		})
	}
}
