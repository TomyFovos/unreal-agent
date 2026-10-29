package tui

import (
	"strconv"
	"strings"
)

// Theme controls only terminal presentation. Its zero value uses 16 colors.
type Theme struct {
	NoColor, Plain, ASCII, Still, Hyperlinks bool
	Depth                                    ColorDepth
}

type ColorDepth uint8

const (
	Color16 ColorDepth = iota
	Color256
	ColorTrue
)

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
	switch getenv("COLORTERM") {
	case "truecolor", "24bit":
		t.Depth = ColorTrue
	default:
		if strings.Contains(getenv("TERM"), "256color") {
			t.Depth = Color256
		}
	}
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
	fg, bg               colorRole
}

type colorRole string

const (
	colorDefault colorRole = "default"
	bandSession  colorRole = "band.session"
	bandOK       colorRole = "band.ok"
	bandWarn     colorRole = "band.warn"
	bandBad      colorRole = "band.bad"
	bandNeutral  colorRole = "band.neutral"
	chipColor    colorRole = "chip"
	chipBad      colorRole = "chip.bad"
	cardColor    colorRole = "card"
	cardAltColor colorRole = "card.alt"
	youBlock     colorRole = "youBlock"
	badgeColor   colorRole = "badge"
	stateRun     colorRole = "s.run"
	stateStall   colorRole = "s.stall"
	stateBad     colorRole = "s.bad"
	stateDone    colorRole = "s.done"
	stateMain    colorRole = "s.main"
	stateYou     colorRole = "s.you"
	dimColor     colorRole = "dim"
)

type terminalColor struct {
	rgb           uint32
	indexed, ansi int
}
type roleColors struct{ fg, bg terminalColor }

var palette = map[colorRole]roleColors{
	bandSession:  {terminalColor{0xEEEDFE, 255, 97}, terminalColor{0x3C3489, 55, 45}},
	bandOK:       {terminalColor{0xE1F5EE, 194, 30}, terminalColor{0x085041, 23, 42}},
	bandWarn:     {terminalColor{0xFAEEDA, 230, 30}, terminalColor{0x633806, 94, 43}},
	bandBad:      {terminalColor{0xFCEBEB, 224, 97}, terminalColor{0x791F1F, 88, 41}},
	bandNeutral:  {terminalColor{0xD3D1C7, 252, 97}, terminalColor{0x2C2C2A, 236, 100}},
	chipColor:    {terminalColor{0xD3D1C7, 252, 97}, terminalColor{0x24242A, 235, 100}},
	chipBad:      {terminalColor{0xFCEBEB, 224, 97}, terminalColor{0x501313, 52, 41}},
	cardColor:    {bg: terminalColor{0x1F1F25, 235, 0}},
	cardAltColor: {bg: terminalColor{0x232329, 236, 0}},
	youBlock:     {bg: terminalColor{0x22222A, 235, 0}},
	badgeColor:   {terminalColor{0xF1EFE8, 255, 97}, terminalColor{0x444441, 238, 100}},
	stateRun:     {fg: terminalColor{0x5DCAA5, 79, 36}},
	stateStall:   {fg: terminalColor{0xFAC775, 222, 33}},
	stateBad:     {fg: terminalColor{0xF09595, 210, 31}},
	stateDone:    {fg: terminalColor{0x97C459, 149, 32}},
	stateMain:    {fg: terminalColor{0xAFA9EC, 147, 35}},
	stateYou:     {fg: terminalColor{0x85B7EB, 111, 36}},
	dimColor:     {fg: terminalColor{0x86847C, 244, 0}},
}

// visualStyle is shared by future bands, chips and cards; legacy layouts keep
// their existing styles until their own redesign step.
func visualStyle(role colorRole) style {
	s := style{fg: role}
	if palette[role].bg != (terminalColor{}) {
		s.bg = role
		if palette[role].fg == (terminalColor{}) {
			s.fg = colorDefault
		}
	}
	return s
}

var (
	normal         = style{}
	meta           = style{faint: true}
	strong         = style{bold: true}
	youStyle       = style{bold: true, fg: stateYou}
	agentStyle     = style{bold: true, fg: stateMain}
	liveStyle      = style{fg: stateMain}
	successStyle   = style{fg: stateDone}
	failureStyle   = style{bold: true, fg: stateBad}
	warningStyle   = style{bold: true, fg: stateStall}
	selectionStyle = style{reverse: true}
)

func (t Theme) sgr(s style) string {
	if t.Plain {
		return ""
	}
	var codes []string
	if s.bold {
		codes = append(codes, "1")
	}
	if s.faint || !t.NoColor && t.Depth == Color16 && s.fg == dimColor {
		codes = append(codes, "2")
	}
	if s.reverse {
		codes = append(codes, "7")
	}
	if !t.NoColor {
		if code := t.colorSGR(s.fg, false); code != "" {
			codes = append(codes, code)
		}
		if code := t.colorSGR(s.bg, true); code != "" {
			codes = append(codes, code)
		}
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func (t Theme) colorSGR(role colorRole, background bool) string {
	if role == "" {
		return ""
	}
	base := 38
	color := palette[role].fg
	if background {
		base = 48
		color = palette[role].bg
	}
	if role == colorDefault {
		return strconv.Itoa(base + 1)
	}
	if color == (terminalColor{}) {
		return ""
	}
	switch t.Depth {
	case ColorTrue:
		return strconv.Itoa(base) + ";2;" + strconv.Itoa(int(color.rgb>>16)) + ";" + strconv.Itoa(int(color.rgb>>8&255)) + ";" + strconv.Itoa(int(color.rgb&255))
	case Color256:
		return strconv.Itoa(base) + ";5;" + strconv.Itoa(color.indexed)
	default:
		if color.ansi != 0 {
			return strconv.Itoa(color.ansi)
		}
		return ""
	}
}

// roleLine builds only the requested section, including the visible fallback
// delimiters before the caller measures or clips it. It never pads a row.
func (t Theme) roleLine(text string, role colorRole) line {
	if (t.NoColor || t.Plain) && (strings.HasPrefix(string(role), "band.") || role == chipColor || role == chipBad) {
		text = "[" + text + "]"
	}
	return textLine(t.chrome(text), visualStyle(role))
}

// chrome is for renderer-owned symbols, never user or tool text.
func (t Theme) chrome(text string) string {
	if !t.ASCII {
		return text
	}
	return strings.NewReplacer("▌", "|", "▁", ".", "▂", ":", "▃", "-", "▅", "=", "▇", "#", "█", "#", "░", "-", "●", "*", "◆", "*", "✓", "+", "✗", "x", "▸", ">").Replace(text)
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
	case "host":
		return meta
	default:
		return strong
	}
}
