package tui

import (
	"strconv"
	"strings"
)

// Theme controls only terminal presentation. No background colors are used.
type Theme struct{ NoColor, Plain, ASCII, Still, Hyperlinks bool }

func EnvironmentTheme(getenv func(string) string) Theme {
	locale := getenv("LC_ALL")
	if locale == "" {
		locale = getenv("LC_CTYPE")
	}
	if locale == "" {
		locale = getenv("LANG")
	}
	locale = strings.ToLower(locale)
	t := Theme{NoColor: getenv("NO_COLOR") != "", Plain: getenv("TERM") == "dumb" || getenv("UNREAL_AGENT_TUI_PLAIN") == "1", ASCII: getenv("UNREAL_AGENT_TUI_ASCII") == "1" || !strings.Contains(strings.ReplaceAll(locale, "-", ""), "utf8"), Still: getenv("UNREAL_AGENT_TUI_NO_ANIMATION") == "1"}
	t.Hyperlinks = !t.Plain && hyperlinkTerminal(getenv)
	return t
}

// Conservative capability detection: an unknown terminal gets visible URLs
// without OSC. Multiplexers require passthrough support we do not assume.
func hyperlinkTerminal(getenv func(string) string) bool {
	term := getenv("TERM")
	if term == "" || term == "dumb" || term == "linux" || term == "vt100" ||
		strings.HasPrefix(term, "screen") || strings.HasPrefix(term, "tmux") || getenv("TMUX") != "" || getenv("STY") != "" {
		return false
	}
	switch getenv("TERM_PROGRAM") {
	case "iTerm.app", "WezTerm", "vscode", "ghostty":
		return true
	}
	if term == "xterm-kitty" || term == "foot" || term == "foot-extra" || getenv("WT_SESSION") != "" {
		return true
	}
	vte, err := strconv.Atoi(getenv("VTE_VERSION"))
	return err == nil && vte >= 5000
}

type style struct {
	bold, faint, reverse bool
	color                int
}

var (
	normal         = style{}
	meta           = style{faint: true}
	strong         = style{bold: true}
	youStyle       = style{bold: true, color: 36}
	agentStyle     = style{bold: true, color: 35}
	liveStyle      = style{color: 35}
	successStyle   = style{color: 32}
	failureStyle   = style{bold: true, color: 31}
	warningStyle   = style{bold: true, color: 33}
	selectionStyle = style{reverse: true}
	privateStyle   = style{bold: true, reverse: true}
)

func (t Theme) sgr(s style) string {
	if t.Plain {
		return ""
	}
	var codes []string
	if s.bold {
		codes = append(codes, "1")
	}
	if s.faint {
		codes = append(codes, "2")
	}
	if s.reverse {
		codes = append(codes, "7")
	}
	if s.color != 0 && !t.NoColor {
		codes = append(codes, strconv.Itoa(s.color))
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}
func (t Theme) symbol(unicode, ascii string) string {
	if t.ASCII {
		return ascii
	}
	return unicode
}
func (t Theme) spinner(nowMillis int64) string {
	if t.Still {
		return t.symbol("▸", ">")
	}
	if t.ASCII {
		return []string{"|", "/", "-", "\\"}[(nowMillis/120)%4]
	}
	return []string{"⠁", "⠈", "⠐", "⠠", "⢀", "⡀", "⠄", "⠂"}[(nowMillis/120)%8]
}

func roleStyle(role string) style {
	switch role {
	case "you":
		return youStyle
	case "agent":
		return agentStyle
	case "error":
		return failureStyle
	case "key":
		return privateStyle
	case "host":
		return meta
	default:
		return strong
	}
}
