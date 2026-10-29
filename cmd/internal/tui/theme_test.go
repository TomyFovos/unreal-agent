package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func TestEnvironmentColorDepth(t *testing.T) {
	for _, tc := range []struct {
		name, colorterm, term string
		depth                 ColorDepth
	}{
		{"truecolor", "truecolor", "xterm-256color", ColorTrue},
		{"24bit", "24bit", "xterm", ColorTrue},
		{"256", "", "screen-256color", Color256},
		{"16", "", "xterm", Color16},
		{"unknown", "", "", Color16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COLORTERM": tc.colorterm, "TERM": tc.term, "LANG": "ja_JP.UTF-8"}
			theme := EnvironmentTheme(func(k string) string { return env[k] })
			if theme.Depth != tc.depth || theme.NoColor || theme.Plain || theme.ASCII {
				t.Fatalf("theme = %+v", theme)
			}
			env["NO_COLOR"] = "1"
			theme = EnvironmentTheme(func(k string) string { return env[k] })
			if got := theme.sgr(style{bold: true, reverse: true, fg: bandSession, bg: bandSession}); got != "\x1b[1;7m" {
				t.Fatalf("NO_COLOR SGR = %q", got)
			}
			env["UNREAL_AGENT_TUI_PLAIN"] = "1"
			theme = EnvironmentTheme(func(k string) string { return env[k] })
			if got := theme.sgr(style{bold: true, reverse: true}); got != "" {
				t.Fatalf("Plain SGR = %q", got)
			}
		})
	}
}

func TestRoleColorSGR(t *testing.T) {
	for _, tc := range []struct {
		role                     colorRole
		truecolor, indexed, ansi string
	}{
		{bandSession, "38;2;238;237;254;48;2;60;52;137", "38;5;255;48;5;55", "97;45"},
		{bandOK, "38;2;225;245;238;48;2;8;80;65", "38;5;194;48;5;23", "30;42"},
		{bandWarn, "38;2;250;238;218;48;2;99;56;6", "38;5;230;48;5;94", "30;43"},
		{bandBad, "38;2;252;235;235;48;2;121;31;31", "38;5;224;48;5;88", "97;41"},
		{bandNeutral, "38;2;211;209;199;48;2;44;44;42", "38;5;252;48;5;236", "97;100"},
		{chipColor, "38;2;211;209;199;48;2;36;36;42", "38;5;252;48;5;235", "97;100"},
		{chipBad, "38;2;252;235;235;48;2;80;19;19", "38;5;224;48;5;52", "97;41"},
		{cardColor, "39;48;2;31;31;37", "39;48;5;235", "39"},
		{youBlock, "39;48;2;34;34;42", "39;48;5;235", "39"},
		{badgeColor, "38;2;241;239;232;48;2;68;68;65", "38;5;255;48;5;238", "97;100"},
		{stateRun, "38;2;93;202;165", "38;5;79", "36"},
		{stateStall, "38;2;250;199;117", "38;5;222", "33"},
		{stateBad, "38;2;240;149;149", "38;5;210", "31"},
		{stateDone, "38;2;151;196;89", "38;5;149", "32"},
		{stateMain, "38;2;175;169;236", "38;5;147", "35"},
		{stateYou, "38;2;133;183;235", "38;5;111", "36"},
		{dimColor, "38;2;134;132;124", "38;5;244", "2"},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			for _, mode := range []struct {
				theme Theme
				want  string
			}{{Theme{Depth: ColorTrue}, tc.truecolor}, {Theme{Depth: Color256}, tc.indexed}, {Theme{}, tc.ansi}, {Theme{NoColor: true}, ""}, {Theme{Plain: true}, ""}} {
				want := mode.want
				if want != "" {
					want = "\x1b[" + want + "m"
				}
				if got := mode.theme.sgr(visualStyle(tc.role)); got != want {
					t.Fatalf("theme %+v: SGR = %q, want %q", mode.theme, got, want)
				}
			}
		})
	}
}

func TestPaintResetsBackgroundAtSectionBoundaries(t *testing.T) {
	a := style{fg: stateYou, bg: cardColor}
	b := style{fg: stateYou, bg: youBlock}
	l := joined(textLine("A", a), textLine("B", b), textLine(" plain", normal))
	want := "\x1b[38;2;133;183;235;48;2;31;31;37mA\x1b[0m\x1b[38;2;133;183;235;48;2;34;34;42mB\x1b[0m plain"
	if got := l.paint(Theme{Depth: ColorTrue}); got != want {
		t.Fatalf("paint = %q, want %q", got, want)
	}
	if got := textLine("A", a).paint(Theme{Depth: ColorTrue}); got != "\x1b[38;2;133;183;235;48;2;31;31;37mA\x1b[0m" {
		t.Fatalf("background leaked past section: %q", got)
	}
	if got := l.paint(Theme{Plain: true}); got != "AB plain" {
		t.Fatalf("plain paint = %q", got)
	}
}

func TestThemeFallbackSymbolsAndSections(t *testing.T) {
	const unicode = "▌▁▂▃▅▇█░●◆✓✗▸"
	if got := (Theme{ASCII: true}).chrome(unicode); got != "|.:-=##-**+x>" {
		t.Fatalf("ASCII symbols = %q", got)
	}
	for _, theme := range []Theme{{}, {Depth: ColorTrue}, {Depth: Color256}, {NoColor: true}, {Plain: true}, {ASCII: true}, {NoColor: true, ASCII: true}, {Plain: true, ASCII: true}} {
		if !theme.ASCII && theme.chrome(unicode) != unicode {
			t.Fatal("Unicode symbols changed")
		}
		for _, role := range []colorRole{bandSession, bandOK, bandWarn, bandBad, bandNeutral, chipColor, chipBad} {
			want := "value"
			if theme.NoColor || theme.Plain {
				want = "[value]"
			}
			if got := theme.roleLine("value", role).plain(); got != want {
				t.Fatalf("theme %+v role %s = %q, want %q", theme, role, got, want)
			}
		}
		if got := theme.roleLine("▌", cardColor).plain(); got != theme.symbol("▌", "|") {
			t.Fatalf("card edge = %q", got)
		}
	}
}

func TestColorModesKeepFrameAndSectionWidths(t *testing.T) {
	for _, theme := range []Theme{{Depth: ColorTrue}, {Depth: Color256}, {}, {NoColor: true}, {Plain: true}, {ASCII: true}, {NoColor: true, ASCII: true}, {Plain: true, ASCII: true}} {
		for _, size := range [][2]int{{215, 50}, {160, 40}, {120, 30}, {80, 24}, {45, 20}} {
			t.Run(fmt.Sprintf("%+v/%dx%d", theme, size[0], size[1]), func(t *testing.T) {
				s, u := idleSnapshot(), childUI(2)
				u.Theme = theme
				s.Entries = []Entry{{Role: "agent", Text: "**題名** 日本語 👩🏽‍💻 é " + strings.Repeat("試験", 100)}}
				for _, row := range RenderFrame(s, u, size[0], size[1]).Lines {
					if width := uniseg.StringWidth(sgrRE.ReplaceAllString(row, "")); width > size[0]-1 {
						t.Fatalf("row width = %d > %d: %q", width, size[0]-1, row)
					}
				}
				row := joined(theme.roleLine(" session ", bandSession), textLine(" ", normal), theme.roleLine(" ✓ Read 日本語 👩🏽‍💻 ", chipColor))
				for width := 1; width <= size[0]; width++ {
					painted := row.clip(width - 1).paint(theme)
					if got := uniseg.StringWidth(sgrRE.ReplaceAllString(painted, "")); got > width-1 {
						t.Fatalf("section width = %d > %d", got, width-1)
					}
				}
			})
		}
	}
}
