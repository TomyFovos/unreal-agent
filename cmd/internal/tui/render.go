package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type Notification struct {
	Text, Kind, Hint string
	Until            time.Time
}
type Sheet struct {
	Title    string
	Lines    []string
	Analysis string
	Offset   int
}

// UIState is disposable presentation state, owned by the terminal event loop.
type UIState struct {
	Theme                 Theme
	Now                   time.Time
	Input                 string
	Cursor                int // byte boundary in Input; never a grapheme interior
	Pasted                bool
	Private               *credential.Reference
	Busy                  bool
	Outbox                string
	Notification          Notification
	Sheet                 *Sheet
	Picker                *Picker
	Scroll, MenuSelection int
	Transcript            *transcriptPage
	MenuOffset            int
	MenuNavigated         bool
	MenuOptions           []commandCandidate
	Parent                *viewer.Row
	Viewer                viewer.PanelSnapshot
	LegacyPanel           string
	View                  ViewMode
	OrchestrationScroll   int
	cache                 *bodyCache
}

type Frame struct {
	Lines                         []string
	CursorX, CursorY              int // zero-based hardware cursor coordinates
	Width, Height                 int
	ConversationHeight, ScrollMax int
	ConversationRows              int
	MenuOffset                    int
	PickerOffset                  int
	SheetOffset                   int
	SheetHeight                   int
	View                          ViewMode
	OrchestrationHeight           int
	OrchestrationOffset           int
	OrchestrationMax              int
}

type displayRow struct {
	content    line
	role, seam string
	label      bool
	bare       bool
}
type layout struct {
	width, height, main, text int
	tiny                      bool
	rail                      int
}

func makeLayout(width, height int) layout {
	w, h := max(0, min(width-1, 511)), max(0, min(height, 200))
	l := layout{width: w, height: h, main: w, tiny: width < 50}
	l.text = min(100, max(1, w-9))
	if l.tiny {
		l.text = max(1, w-2)
	}
	return l
}
func (l layout) gutter(label, seam string, text line) line {
	if l.tiny {
		if label != "" {
			prefix := label + " "
			if seam != "" {
				prefix += seam + " "
			}
			return joined(textLine(prefix, roleStyle(label)), text).clip(l.main)
		}
		return joined(textLine(seam+strings.Repeat(" ", max(0, 2-uniseg.StringWidth(seam))), meta), text).clip(l.main)
	}
	label = SafeText(label)
	seamStyle := seamStyle(seam)
	if seam == "›" || seam == ">" {
		switch label {
		case "you":
			seamStyle = youStyle
		case "cmd":
			seamStyle = strong
		case "key":
			seamStyle = privateStyle
		case "child":
			seamStyle = strong
		}
	}
	return joined(textLine(strings.Repeat(" ", max(0, 6-uniseg.StringWidth(label)))+label, roleStyle(label)), textLine(" ", normal), textLine(fmt.Sprintf("%-1s", seam), seamStyle), textLine(" ", normal), text).clip(l.main)
}
func seamStyle(s string) style {
	switch s {
	case "✗", "x":
		return failureStyle
	case "!", "?":
		return warningStyle
	case "✓", "+":
		return successStyle
	case "▸", ">":
		return liveStyle
	case "⠁", "⠈", "⠐", "⠠", "⢀", "⡀", "⠄", "⠂", "|", "/", "\\":
		return liveStyle
	default:
		return meta
	}
}
func (l layout) row(r displayRow) line {
	if r.bare {
		return r.content.clip(l.main)
	}
	label := ""
	if r.label && !l.tiny {
		label = r.role
	}
	return l.gutter(label, r.seam, r.content)
}

func connection(s Snapshot, now time.Time) string {
	if s.Connected {
		return "connected"
	}
	if strings.Contains(s.Status, "resync") || strings.Contains(s.Status, "gap") {
		return "resyncing"
	}
	if s.Status == "connecting" {
		return "connecting"
	}
	if !s.OfflineSince.IsZero() && now.Sub(s.OfflineSince) < 2*time.Second {
		return "reconnecting"
	}
	return "disconnected"
}
func runtime(s Snapshot) string {
	if !s.Connected {
		return "unknown"
	}
	if s.Failure != "" || strings.Contains(s.Status, "failed") {
		return "failed"
	}
	if !s.Running {
		return "stopped"
	}
	return "running"
}
func duration(d time.Duration) string {
	n := max(0, int(d.Seconds()))
	if n < 60 {
		return fmt.Sprintf("%ds", n)
	}
	if n < 3600 {
		return fmt.Sprintf("%dm%02ds", n/60, n%60)
	}
	return fmt.Sprintf("%dh%02dm", n/3600, n/60%60)
}
func number(n int64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fm", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
func usage(u viewer.Usage) string {
	if !u.Known {
		return ""
	}
	prefix := ""
	if u.Partial {
		prefix = "~"
	}
	field := func(v int64, f llm.UsageField) string {
		if v == 0 && slices.Contains(u.Unknown, f) {
			return "unknown"
		}
		return number(v)
	}
	return prefix + "in " + field(u.Input, llm.UsageInput) + " out " + field(u.Output, llm.UsageOutput)
}
func header(s Snapshot, u UIState, l layout) line {
	conn, run := connection(s, u.Now), runtime(s)
	cs, rs := meta, meta
	if conn == "disconnected" {
		cs = failureStyle
	} else if conn != "connected" {
		cs = warningStyle
	}
	if run == "failed" {
		rs = failureStyle
	} else if run == "unknown" {
		rs = warningStyle
	} else if run == "stopped" {
		rs = strong
	}
	elapsed, tokens := "", ""
	model := ""
	if s.Selection != nil && l.width >= 69 {
		model = topologyText(selectionLabel(*s.Selection, u.Theme), 256)
	}
	if u.Parent != nil {
		if s.Connected && runtime(s) == "running" && u.Parent.Elapsed.Known {
			elapsed = duration(u.Parent.Elapsed.Value)
		}
		if value := usage(u.Parent.Usage); value != "" {
			tokens = value
		}
	}
	shortTokens := func() {
		if u.Parent != nil && u.Parent.Usage.Known {
			prefix := ""
			if u.Parent.Usage.Partial {
				prefix = "~"
			}
			tokens = prefix + number(u.Parent.Usage.Input) + "/" + number(u.Parent.Usage.Output)
		}
	}
	connText, runText := conn, run
	if conn == "connected" {
		if !u.Theme.ASCII {
			connText = "⇄ connected"
		}
	} else if conn == "disconnected" {
		connText = u.Theme.symbol("✗", "x") + " " + conn
	} else {
		connText = u.Theme.spinner(u.Now.UnixMilli()) + " " + conn
	}
	if run == "unknown" {
		runText = "? unknown"
	} else if run == "stopped" {
		runText = u.Theme.symbol("▪", "#") + " stopped"
	} else if run == "failed" {
		runText = u.Theme.symbol("✗", "x") + " failed"
	}
	if l.width < 69 {
		shortTokens()
		if conn == "connected" && !u.Theme.ASCII {
			connText = "⇄"
		}
	}
	id := SafeText(string(s.ID))
	brand := ""
	if l.width >= 69 {
		brand = "unreal agent  "
	}
	build := func() line {
		metric := ""
		if model != "" {
			metric += "  " + model
		}
		if elapsed != "" {
			metric += "  " + elapsed
		}
		if tokens != "" {
			metric += "  " + tokens
		}
		return joined(textLine(brand, agentStyle), textLine(id, strong), textLine("  "+u.View.effective(l.width+1).title(), meta), textLine("  "+connText, cs), textLine("  "+runText, rs), textLine(metric, meta))
	}
	if build().width() > l.width {
		model = ""
	}
	if build().width() > l.width {
		brand = ""
	}
	if build().width() > l.width {
		shortTokens()
	}
	if build().width() > l.width {
		elapsed = ""
	}
	if build().width() > l.width {
		tokens = ""
	}
	if l.width < 39 && build().width() > l.width && run == "running" {
		runText = ""
	}
	if build().width() > l.width && conn == "connected" && !u.Theme.ASCII {
		connText = "⇄"
	}
	if build().width() > l.width {
		keep := 8
		if l.width < 39 {
			keep = 1
		}
		id = middle(id, max(keep, l.width-textLine(connText, cs).width()-textLine(runText, rs).width()-textLine(u.View.effective(l.width+1).title(), meta).width()-6), u.Theme)
	}
	return build().clip(l.width)
}

func conversation(s Snapshot, u UIState, l layout) []displayRow {
	entries := s.Entries
	if u.Transcript != nil {
		entries = u.Transcript.Entries
		s.OlderDropped = u.Transcript.Older
	}
	if len(entries) == 0 && len(s.Lines) > 0 {
		for _, v := range s.Lines {
			role, text := "agent", v
			if before, after, ok := strings.Cut(v, "> "); ok {
				role, text = before, after
			}
			entries = append(entries, Entry{Role: role, Text: SafeText(text)})
		}
	}
	var out []displayRow
	if s.OlderDropped {
		out = append(out, displayRow{content: textLine(olderMessage, meta)})
	}
	lastRole := ""
	for _, e := range entries {
		if e.PeerID != "" {
			for _, child := range children(u, s) {
				if string(child.ID) == e.PeerID {
					e.Role = "child"
					break
				}
			}
		}
		if len(out) > 0 {
			out = append(out, displayRow{})
		}
		show := e.Role != lastRole
		if l.tiny && show {
			out = append(out, displayRow{content: textLine(e.Role, roleStyle(e.Role)), bare: true})
		}
		var content []bodyLine
		if e.Text != "" || len(e.Calls) == 0 {
			if u.cache != nil {
				content = u.cache.get(e, l.text, u.Theme)
			} else {
				content = entryBody(e, l.text, u.Theme)
			}
		}
		if l.tiny && len(e.Calls) > 0 {
			n := len(e.Calls)
			spawns := 0
			for _, c := range e.Calls {
				if c.Name == "SubagentStart" {
					spawns++
				}
			}
			done, failed, canceled, active := 0, 0, 0, 0
			for _, raw := range e.Calls {
				r, _ := currentReceipt(raw, s, u)
				switch r.Status {
				case operation.StatusCompleted:
					done++
				case operation.StatusFailed:
					failed++
				case operation.StatusCanceled:
					canceled++
				default:
					active++
				}
			}
			mark := u.Theme.symbol("▸", ">")
			if failed > 0 {
				mark = u.Theme.symbol("✗", "x")
			} else if active == 0 && canceled == 0 {
				mark = u.Theme.symbol("✓", "+")
			}
			counts := fmt.Sprintf("%d running, %d done", active, done)
			if failed > 0 {
				counts += fmt.Sprintf(", %d failed", failed)
			}
			if canceled > 0 {
				counts += fmt.Sprintf(", %d canceled", canceled)
			}
			text := fmt.Sprintf("%s %d tool calls: %s", mark, n, counts)
			if spawns > 0 {
				text = fmt.Sprintf("%s %d spawns: %s", mark, spawns, counts)
			}
			content = append(content, bodyLine{text: textLine(middle(text, l.text, u.Theme), meta)})
		} else {
			content = append(content, toolRows(e, s, u, l)...)
		}
		for i, b := range content {
			out = append(out, displayRow{content: b.text, role: e.Role, seam: b.seam, label: show && i == 0})
		}
		lastRole = e.Role
	}
	if u.Transcript == nil && s.Connected && s.Running && s.Progress != nil && s.Progress.Mode == "streaming" && s.Progress.Text != "" {
		if len(out) > 0 {
			out = append(out, displayRow{})
		}
		show := lastRole != "agent"
		if l.tiny && show {
			out = append(out, displayRow{content: textLine("agent", agentStyle), bare: true})
		}
		for i, b := range entryBody(Entry{Role: "draft", Text: s.Progress.Text}, l.text, u.Theme) {
			out = append(out, displayRow{content: b.text, role: "agent", seam: u.Theme.symbol("⋮", ":"), label: show && i == 0})
		}
	}
	if len(out) == 0 && !s.Connected {
		out = append(out, displayRow{content: textLine("connecting to the host", meta)})
	}
	return out
}

func composer(s Snapshot, u UIState, l layout) ([]line, int, int, int, string) {
	label := "you"
	command := strings.HasPrefix(u.Input, "/") && !u.Pasted && u.Private == nil
	if command {
		label = "cmd"
	}
	input, cursor := u.Input, min(max(0, u.Cursor), len(u.Input))
	if u.Private != nil {
		label = "key"
		cursor = min(64, uniseg.GraphemeClusterCount(input[:cursor]))
		input = strings.Repeat("*", min(64, uniseg.GraphemeClusterCount(input)))
	}
	placeholder := ""
	if input == "" {
		placeholder = "Ask, or type / for commands"
		if child := focus(u, s); child != "" {
			placeholder = "Message " + string(s.ID) + ", or /child-send TEXT for " + string(child)
		}
		switch {
		case u.Private != nil:
			placeholder = "Type or paste the key; it stays masked"
		case !s.Connected && s.Status == "connecting":
			placeholder = "Connecting to the host"
		case !s.Connected:
			placeholder = "Offline; you can keep typing"
		case runtime(s) == "stopped":
			placeholder = "Session stopped; /resume continues it"
		case runtime(s) == "failed":
			placeholder = "Session failed; see above"
		}
	}
	var parts []line
	row, col, pos, cursorRow, cursorCol := 0, 0, 0, 0, 0
	current := line(nil)
	g := uniseg.NewGraphemes(SafeText(input))
	for g.Next() {
		start, end := g.Positions()
		str, w := g.Str(), g.Width()
		if str == "\t" {
			str = "    "
			w = 4
		}
		if str != "\n" && col+w > l.text {
			parts = append(parts, current)
			current = nil
			row++
			col = 0
		}
		if cursor >= start && cursor < end {
			cursorRow, cursorCol = row, col
		}
		if str == "\n" {
			parts = append(parts, current)
			current = nil
			row++
			col = 0
		} else {
			if w > l.text {
				str = "�"
				w = 1
			}
			current = append(current, textLine(str, normal)...)
			col += w
		}
		pos = end
		if cursor == end {
			cursorRow, cursorCol = row, col
		}
	}
	if pos == 0 {
		cursorRow, cursorCol = 0, 0
	}
	parts = append(parts, current)
	if cursorCol >= l.text {
		cursorRow++
		cursorCol = 0
	}
	for cursorRow >= len(parts) {
		parts = append(parts, nil)
	}
	limit := max(1, min(8, l.height/5))
	start := max(0, cursorRow-limit+1)
	end := min(len(parts), start+limit)
	if placeholder != "" {
		parts[0] = textLine(placeholder, meta).clip(l.text)
	}
	rows := make([]line, 0, end-start)
	for i, p := range parts[start:end] {
		role, seam := "", ""
		if i == 0 {
			role = label
			seam = u.Theme.symbol("›", ">")
		}
		if l.tiny {
			role = ""
		}
		rows = append(rows, l.gutter(role, seam, p))
	}
	offset := 9
	if l.tiny {
		offset = 2
	}
	return rows, cursorRow - start, min(l.main-1, offset+cursorCol), len(parts) - (end - start), label
}

func rule(u UIState, l layout, overflow int, label string) line {
	value := ""
	switch {
	case u.Picker != nil:
		value = "selection; Esc back/cancel"
	case u.Sheet != nil && u.Sheet.Analysis != "":
		value = "PgUp/PgDn scroll  Esc/Enter back  ^D detach"
	case u.Private != nil:
		value = "private: API key for " + u.Private.Provider + "/" + u.Private.ID
	case u.Pasted:
		value = "pasted: Enter submits; registered commands run"
	case label == "cmd":
		value = "commands"
	case u.View.effective(l.width+1) == ViewOrchestration:
		value = fmt.Sprintf("Orchestration: line %d; PgUp/PgDn scroll", u.OrchestrationScroll+1)
	case u.View == ViewSplit && l.width < 119:
		value = "Chat; Split returns at 120 columns"
	case u.Scroll > 0 || u.Transcript != nil:
		value = fmt.Sprintf("scrollback: %d lines below; PgDn returns", u.Scroll)
		if u.Transcript != nil && u.Transcript.Newer {
			value = fmt.Sprintf("scrollback: %d lines below in page; PgDn continues", u.Scroll)
		}
	case u.Viewer.Selected != "" && u.Viewer.Selected != u.Viewer.ParentID:
		value = "focus " + string(u.Viewer.Selected)
	}
	if l.tiny && value == "" {
		value = label
	}
	if overflow > 0 {
		if value != "" {
			value += "  "
		}
		value += fmt.Sprintf("+%d lines", overflow)
	}
	if len(u.Input) >= 48<<10 && u.Private == nil {
		value += fmt.Sprintf("  %d KB of 64 KB", len(u.Input)/1024)
	}
	mark := u.Theme.symbol("─", "-")
	if value == "" {
		return textLine(strings.Repeat(mark, l.width), meta)
	}
	value = middle(value, max(0, l.width-4), u.Theme)
	return textLine(mark+" "+value+" "+strings.Repeat(mark, max(0, l.width-uniseg.StringWidth(value)-3)), meta).clip(l.width)
}
func keybar(s Snapshot, u UIState, l layout, label string) line {
	enter, tab := u.Theme.symbol("⏎", "enter"), u.Theme.symbol("⇥", "tab")
	value := enter + " send   / commands   ^C stop session   ^D detach"
	switch {
	case u.Picker != nil:
		value = u.Theme.symbol("↑↓", "up/down") + " select  " + enter + " continue  Esc back/cancel  ^D detach"
		if u.Picker.Kind == "response-export" {
			value = u.Theme.symbol("↑↓", "up/down") + " select  " + enter + " export  Esc cancel  ^D detach"
		}
		if u.Picker.Kind == "view" {
			value = u.Theme.symbol("↑↓", "up/down") + " select  " + enter + " apply  Esc cancel  ^D detach"
		}
	case u.Sheet != nil && u.Sheet.Analysis != "":
		value = "PgUp/PgDn scroll  Esc/Enter back  ^D detach"
	case u.Private != nil:
		value = enter + " store key   ^C cancel entry   ^D detach"
	case u.Busy:
		value = "^C cancel waiting   ^D detach"
	case label == "cmd":
		value = tab + " complete   " + enter + " run   ^C stop session   ^D detach"
		if len(menuOptions(s, u)) > 0 {
			value = u.Theme.symbol("↑↓", "up/down") + " select   " + tab + " complete   " + enter + " choose   ^C stop   ^D detach"
			if l.width < 69 {
				value = u.Theme.symbol("↑↓ select  "+tab+" complete  "+enter+" choose", "^v  tab fill  enter choose") + " ^C stop ^D detach"
			}
		}
	case u.View.effective(l.width+1) == ViewOrchestration:
		value = "PgUp/PgDn scroll   /view chat   " + enter + " send   ^C stop   ^D detach"
	case !s.Connected && s.Status == "connecting":
		value = enter + " send   ^D detach"
	case !s.Connected:
		value = enter + " send (fails until reconnected)   ^D detach"
	case runtime(s) == "stopped":
		value = "/resume continue   ^D detach"
	case runtime(s) == "failed":
		value = "/resume   ^D detach"
	case u.Scroll > 0:
		value = "PgUp/PgDn scroll   " + enter + " send and return to live"
	case focus(u, s) != "":
		value = enter + " send to " + string(s.ID) + "   /child-send TEXT   ^C stop session"
	case l.width < 69:
		value = enter + " send  / cmds  ^C stop  ^D detach"
	case l.width >= 119:
		value += "   PgUp/PgDn scroll"
	}
	return l.gutter("", "", textLine(value, meta)).clip(l.width)
}

func RenderFrame(s Snapshot, u UIState, width, height int) Frame {
	l := makeLayout(width, height)
	if u.Now.IsZero() {
		u.Now = time.Now()
	}
	if u.Parent == nil {
		for i := range u.Viewer.Rows {
			if u.Viewer.Rows[i].ID == s.ID {
				u.Parent = &u.Viewer.Rows[i]
				break
			}
		}
	}
	f := Frame{Width: l.width, Height: l.height}
	f.View = u.View.effective(width)
	if l.height == 0 {
		return f
	}
	if width < 20 || height < 6 {
		mark := u.Theme.symbol("⇄", "+")
		if !s.Connected {
			mark = "?"
		}
		run := u.Theme.symbol("▸", ">")
		if runtime(s) == "stopped" {
			run = u.Theme.symbol("▪", "#")
		}
		if runtime(s) == "failed" {
			run = u.Theme.symbol("✗", "x")
		}
		if runtime(s) == "unknown" {
			run = "?"
		}
		f.Lines = append(f.Lines, textLine(mark+" "+run, meta).clip(l.width).paint(u.Theme))
		if l.height > 1 {
			for len(f.Lines) < l.height-1 {
				f.Lines = append(f.Lines, "")
			}
			text := u.Input
			if u.Private != nil {
				text = strings.Repeat("*", min(64, uniseg.GraphemeClusterCount(text)))
			}
			f.Lines = append(f.Lines, textLine(text, normal).clip(l.width).paint(u.Theme))
			f.CursorX = min(max(0, l.width-1), uniseg.StringWidth(text))
			f.CursorY = l.height - 1
		}
		return f
	}
	inputLayout := l
	shortHint := ""
	if height < 16 {
		shortHint = "^C stop"
		if u.Private != nil {
			shortHint = "^C cancel entry"
		} else if u.Busy {
			shortHint = "^C cancel waiting"
		}
		inputLayout.text = max(1, l.text-len(shortHint)-2)
	}
	input, cy, cx, overflow, label := composer(s, u, inputLayout)
	if shortHint != "" {
		i := len(input) - 1
		input[i] = joined(input[i], textLine(strings.Repeat(" ", max(1, l.main-input[i].width()-len(shortHint)))+shortHint, meta)).clip(l.main)
	}
	full := l
	if f.View == ViewSplit {
		l.rail = max(34, min(44, width*3/10))
		if width >= 160 {
			l.rail = max(48, min(60, width*3/10))
		}
		l.main = l.width - l.rail - 3
		l.text = min(100, max(1, l.main-9))
	} else if f.View == ViewChat && width >= 120 && len(children(u, s)) > 0 {
		l.rail = max(34, min(48, width*3/10))
		if width >= 160 {
			l.rail = 48
		}
		l.main = l.width - l.rail - 3
		l.text = min(100, max(1, l.main-9))
	}
	if u.cache != nil {
		u.cache.begin(l.text, u.Theme.ASCII)
		defer u.cache.end()
	}
	footer := 1 + len(input)
	if l.height >= 16 {
		footer++
	}
	dockState, dockUI := s, u
	if f.View == ViewOrchestration {
		if runtime(s) == "failed" {
			dockState.Failure = "provider/session failure; /analyze Errors for details"
		}
		if dockUI.Busy {
			dockUI.Outbox = "request pending"
		}
	}
	states := stateDock(dockState, dockUI, l)
	notifications := notificationDock(dockUI, l)
	blank := 0
	if l.height >= 20 {
		blank = 1
	}
	minimum := 3
	if l.height >= 20 {
		minimum = l.height * 2 / 5
	}
	maxDock := max(0, l.height-1-blank-footer-minimum)
	// Sheets and child detail yield room to state/errors above the Rule. Keeping
	// an old conversation on screen never hides an uncertain-send retry notice.
	if len(states)+len(notifications) > maxDock {
		states = states[:min(len(states), max(0, maxDock-len(notifications)))]
	}
	if len(notifications) > maxDock {
		notifications = notifications[:maxDock]
	}
	available := max(0, maxDock-len(states)-len(notifications))
	clipDock := func(rows []line, limit int, title string) []line {
		if len(rows) <= limit {
			return rows
		}
		if limit == 0 {
			return nil
		}
		hidden := len(rows) - limit + 1
		rows = rows[:limit]
		rows[len(rows)-1] = l.gutter(title, "", textLine(fmt.Sprintf("+%d more; type / to search", hidden), meta))
		return rows
	}
	dock := childDock(s, u, l, u.Sheet != nil || u.Picker != nil || label == "cmd")
	if f.View != ViewChat {
		dock = nil
	}
	if u.Picker != nil || u.Sheet != nil && u.Sheet.Analysis != "" {
		dock = nil
	}
	childBudget := available
	if len(menuOptions(s, u)) > 0 {
		childBudget = max(0, available-1)
	}
	dock = clipDock(dock, childBudget, "")
	if u.Sheet == nil && u.Picker == nil {
		// Give the menu its actual row budget. A later generic truncation must
		// never replace a selected command with a hidden-row count.
		commands, offset := commandSheetWithin(s, u, l, max(0, available-len(dock)))
		f.MenuOffset = offset
		dock = append(dock, commands...)
	}
	var extra []line
	if u.Picker != nil {
		extra, f.PickerOffset = pickerRows(u.Picker, u, l, available)
	}
	if u.LegacyPanel != "" {
		for _, v := range strings.Split(SafeText(u.LegacyPanel), "\n") {
			extra = append(extra, l.gutter("", "", textLine(v, meta)))
		}
	}
	title := ""
	if u.Sheet != nil {
		title = u.Sheet.Title
		if u.Theme.ASCII {
			title = strings.ReplaceAll(title, " · ", " / ")
		}
		if u.Sheet.Analysis != "" {
			budget := max(0, available-len(dock))
			if budget > 0 {
				extra = append(extra, l.gutter("", "", textLine(title, strong).clip(l.text)))
			}
			limit := max(0, budget-1)
			f.SheetHeight = limit
			var wrapped []string
			for _, v := range u.Sheet.Lines {
				wrapped = append(wrapped, Wrap(v, l.text)...)
			}
			start := min(u.Sheet.Offset, max(0, len(wrapped)-max(1, limit)))
			f.SheetOffset = start
			for _, v := range wrapped[start:min(len(wrapped), start+limit)] {
				extra = append(extra, l.gutter("", "", textLine(v, normal).clip(l.text)))
			}
		} else {
			limit := max(3, min(10, l.height/3))
			limit = min(limit, max(0, available-len(dock)))
			f.SheetHeight = limit
			start := min(u.Sheet.Offset, max(0, len(u.Sheet.Lines)-max(1, limit)))
			f.SheetOffset = start
			for i, v := range u.Sheet.Lines[start:min(len(u.Sheet.Lines), start+limit)] {
				role := ""
				if i == 0 {
					role = title
				}
				extra = append(extra, l.gutter(role, "", textLine(v, normal)))
			}
		}
	}
	dock = append(dock, clipDock(extra, max(0, available-len(dock)), title)...)
	dock = append(dock, states...)
	dock = append(dock, notifications...)
	gap := 0
	if l.height >= 24 && len(dock) > 0 && len(dock) < maxDock {
		gap = 1
	}
	room := max(0, l.height-1-blank-footer-len(dock)-gap)
	pulseRow := pulse(s, u, l)
	if f.View == ViewOrchestration {
		pulseRow = nil
	}
	conversationRoom := room
	if pulseRow != nil {
		conversationRoom = max(0, room-1)
	}
	f.ConversationHeight = conversationRoom
	var body []displayRow
	if f.View != ViewOrchestration {
		body = conversation(s, u, l)
	}
	f.ConversationRows = len(body)
	f.ScrollMax = max(0, len(body)-conversationRoom)
	scroll := min(max(0, u.Scroll), f.ScrollMax)
	end := max(0, len(body)-scroll)
	start := max(0, end-conversationRoom)
	visible := body[start:end]
	if len(visible) > 0 && start > 0 {
		for i := range visible {
			if visible[i].role != "" {
				visible = append([]displayRow(nil), visible...)
				visible[i].label = true
				// Tiny already has role rows in the transcript. Replacing the
				// first visible body row with a repeated label loses a real line
				// on every page. Use its reserved seam without changing the body.
				if l.tiny && !visible[i].bare {
					visible[i].seam = u.Theme.symbol("↪", ">")
				}
				break
			}
		}
	}
	rows := []line{header(s, u, l)}
	if blank > 0 {
		rows = append(rows, nil)
	}
	var topology OrchestrationSnapshot
	if f.View != ViewChat {
		topology = BuildOrchestration(s, u.Viewer, u.Now, max(64, l.height*8))
	}
	if f.View == ViewOrchestration {
		var pane []line
		f.OrchestrationHeight = conversationRoom
		pane, f.OrchestrationOffset, f.OrchestrationMax = orchestrationPane(topology, u, l.main, conversationRoom, false)
		u.OrchestrationScroll = f.OrchestrationOffset
		rows = append(rows, pane...)
	} else {
		for _, r := range visible {
			rows = append(rows, l.row(r))
		}
	}
	for len(rows) < 1+blank+conversationRoom {
		rows = append(rows, nil)
	}
	if pulseRow != nil {
		rows = append(rows, pulseRow)
	}
	if gap > 0 {
		rows = append(rows, nil)
	}
	rows = append(rows, dock...)
	if l.rail > 0 {
		railTop := 1 + blank
		var rail []line
		if f.View == ViewSplit {
			railTop = 1
			if blank > 0 {
				rows[1] = textLine("Chat", strong)
			}
			f.OrchestrationHeight = len(rows) - railTop
			rail, f.OrchestrationOffset, f.OrchestrationMax = orchestrationPane(topology, u, l.rail, f.OrchestrationHeight, true)
		} else {
			rail = childRail(s, u, l, len(rows)-railTop)
		}
		for i, right := range rail {
			at := railTop + i
			left := rows[at].clip(l.main)
			rows[at] = joined(left, textLine(strings.Repeat(" ", l.main-left.width()+3), normal), right.clip(l.rail))
		}
	}
	if l.height >= 16 {
		rows = append(rows, keybar(s, u, full, label))
	}
	rows = append(rows, rule(u, full, overflow, label))
	f.CursorY = len(rows) + cy
	f.CursorX = max(0, cx)
	rows = append(rows, input...)
	for _, r := range rows[:min(len(rows), l.height)] {
		f.Lines = append(f.Lines, r.clip(l.width).paint(u.Theme))
	}
	f.CursorY = min(f.CursorY, l.height-1)
	return f
}

func selectionLabel(s sessionstore.RuntimeSelection, t Theme) string {
	name := s.Name
	if name == "" {
		name = s.Model
	}
	provider := modelcatalog.ProviderName(s.Provider)
	if s.Provider == "openai-codex" {
		provider = "Codex"
	}
	if provider != "" {
		name = provider + t.symbol(" · ", " / ") + name
	}
	if s.Effort == "" {
		return SafeText(name)
	}
	return SafeText(name) + t.symbol(" · ", " / ") + string(s.Effort)
}

// Render is retained for embedders of the old string hook. The interactive path
// uses structured RenderFrame and a differential terminal renderer.
func Render(s Snapshot, input, status string, width, height int, panel string) string {
	u := UIState{Theme: Theme{Plain: true}, Input: strings.TrimPrefix(input, "> "), Notification: Notification{Text: status}, LegacyPanel: panel}
	u.Cursor = len(u.Input)
	f := RenderFrame(s, u, width, height)
	return "\x1b[H" + strings.Join(f.Lines, "\r\n")
}
