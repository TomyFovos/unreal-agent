package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/displaytext"
	"github.com/unreallabsai/unreal-agent/internal/usertext"
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
	PermissionDismissed   string
	Theme                 Theme
	Now                   time.Time
	Input                 string
	Cursor                int // byte boundary in Input; never a grapheme interior
	Pasted                bool
	Selected, Secret      bool
	HistoryLoading        bool
	Busy                  bool
	Outbox                string
	Notification          Notification
	Sheet                 *Sheet
	Picker                *Picker
	Scroll, MenuSelection int
	HistoryTop            bool
	Transcript            *transcriptPage
	MenuOffset            int
	MenuNavigated         bool
	MenuOptions           []commandCandidate
	Parent                *viewer.Row
	Viewer                viewer.PanelSnapshot
	View                  ViewMode
	OrchestrationScroll   int
	cache                 *bodyCache
	index                 *renderIndex
	collect               *conversationCache
}

type Frame struct {
	Lines                            []string
	CursorX, CursorY                 int // zero-based hardware cursor coordinates
	Width, Height                    int
	ConversationHeight, ScrollMax    int
	ConversationRows                 int
	ConversationY, ConversationWidth int
	MenuOffset                       int
	PickerOffset                     int
	SheetOffset                      int
	SheetHeight                      int
	View                             ViewMode
	OrchestrationHeight              int
	OrchestrationOffset              int
	OrchestrationMax                 int
}

type displayRow struct {
	content    line
	role, seam string
}
type layout struct {
	width, height, main, text int
	tiny                      bool
	rail                      int
}

func makeLayout(width, height int) layout {
	w, h := max(0, min(width-1, 511)), max(0, min(height, 200))
	l := layout{width: w, height: h, main: w, tiny: width < 50}
	l.text = min(100, max(1, w-2))
	return l
}
func (l layout) gutter(label, seam string, text line) line {
	if label != "" {
		text = joined(textLine(SafeText(label)+" ", roleStyle(label)), text)
	}
	return l.markedGutter(seam, seamStyle(seam), text)
}
func (l layout) markedGutter(mark string, color style, text line) line {
	return joined(textLine(mark+strings.Repeat(" ", max(0, 2-uniseg.StringWidth(mark))), color), text).clip(l.main)
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
	mark, color := r.seam, seamStyle(r.seam)
	switch r.role {
	case "you":
		mark, color = r.seam, visualStyle(stateYou)
	case "agent":
		color = visualStyle(stateMain)
	case "draft":
		color = visualStyle(dimColor)
	case "error":
		color = visualStyle(stateBad)
	case "host":
		color = visualStyle(dimColor)
	}
	return l.markedGutter(mark, color, r.content)
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
	name := SafeText(string(s.ID))
	state, role := "入力待ち", bandNeutral
	elapsed := ""
	switch connection(s, u.Now) {
	case "connecting":
		state, role = u.Theme.spinner(u.Now.UnixMilli())+" 接続中", bandWarn
	case "reconnecting":
		state, role = u.Theme.spinner(u.Now.UnixMilli())+" 再接続中", bandWarn
	case "resyncing":
		state, role = u.Theme.spinner(u.Now.UnixMilli())+" 再同期中", bandWarn
	case "disconnected":
		state, role = "✗ 切断", bandBad
	default:
		switch runtime(s) {
		case "stopped":
			state, role = u.Theme.symbol("▪", "#")+" 停止", bandNeutral
		case "failed":
			state, role = "✗ 失敗", bandBad
		default:
			if mainBusy(s) {
				state, role = "● 稼働中", bandOK
			}
			if backgroundActive(s, u) {
				state, role = u.Theme.spinner(u.Now.UnixMilli())+" "+backgroundLabel(workingChildren(s, u)), bandOK
			}
			if s.Progress != nil && s.Progress.Mode == "compacting" {
				state, role = u.Theme.spinner(u.Now.UnixMilli())+" 会話を圧縮中", bandOK
			}
			if u.Parent != nil && u.Parent.Elapsed.Known {
				elapsed = " " + coarseDuration(u.Parent.Elapsed.Value)
			}
		}
	}
	permissions, shortPermission, permissionRole := "", "", bandNeutral
	if s.Permissions.Enabled {
		switch s.Permissions.Mode {
		case permission.FullAccess:
			permissions, shortPermission, permissionRole = "Full Access", "FULL", bandWarn
		case permission.Ask:
			permissions, shortPermission = "Ask", "ASK"
		case permission.ReadOnly:
			permissions, shortPermission = "Read Only", "RO"
		}
	}
	model := ""
	if s.Selection != nil {
		model = headerModelLabel(*s.Selection, u.Theme)
	}
	build := func() line {
		out := u.Theme.roleLine(name, bandSession)
		for _, part := range []struct {
			text string
			role colorRole
		}{{state + elapsed, role}, {permissions, permissionRole}, {model, bandNeutral}} {
			if part.text != "" {
				out = joined(out, textLine(" ", normal), u.Theme.roleLine(part.text, part.role))
			}
		}
		return out
	}
	if build().width() > l.width {
		model = ""
	}
	if build().width() > l.width {
		elapsed = ""
	}
	if build().width() > l.width {
		permissions = shortPermission
	}
	if row := build(); row.width() > l.width {
		name = middle(name, max(0, textLine(name, normal).width()-(row.width()-l.width)), u.Theme)
	}
	return build().clip(l.width)
}

func headerModelLabel(s sessionstore.RuntimeSelection, t Theme) string {
	name := s.Name
	if name == "" {
		name = s.Model
	}
	provider := modelcatalog.ProviderName(s.Provider)
	switch s.Provider {
	case "openai-codex":
		provider = "Codex"
	case "claude-code":
		provider = "Claude"
	}
	if provider != "" && !strings.HasPrefix(name, provider) {
		name = provider + " " + name
	}
	if s.Effort != "" {
		name += t.symbol(" · ", " / ") + string(s.Effort)
	}
	return topologyText(name, 256)
}

func conversation(s Snapshot, u UIState, l layout) []displayRow {
	entries := s.Entries
	if u.Transcript != nil {
		entries = u.Transcript.Entries
		s.OlderDropped = u.Transcript.Older
	}
	var out []displayRow
	switch {
	case u.HistoryLoading:
		out = append(out, displayRow{content: textLine("古い履歴を読み込み中", meta)})
	case s.OlderDropped:
		out = append(out, displayRow{content: textLine(olderMessage, meta)})
	case s.Connected && s.LazyHistory:
		out = append(out, displayRow{content: textLine("ここが会話の始まり", meta)})
	}
	gap := func() {
		if len(out) > 0 && len(out[len(out)-1].content) > 0 {
			out = append(out, displayRow{})
		}
	}
	lastRole, background := "", s.HistoryBackground
	work := backgroundPrefix(s.HistoryWorkPrefix)
	marks := marksFor(u, s)
	flush := func(active bool) {
		if work.entries > 0 {
			gap()
			out = append(out, displayRow{content: work.rowWithMark(marks, l.text, u.Theme, active, u.Now.UnixMilli())})
			lastRole = "background"
			work = backgroundWork{}
		}
	}
	for _, e := range entries {
		if e.Role == "error" && s.Selection != nil {
			e.Text = usertext.Failure(e.Code, e.Text, s.Selection.Provider)
			e.Code = "user_message"
		}
		switch e.Role {
		case "you":
			background = false
		case "peer", "child":
			// A child's ready or progress note does not start background
			// work: the reply that started the child is still the user's turn.
			// Progress still counts as a report in the folded line.
			if e.PeerKind == "ready" {
				continue
			}
			if !quietPeerKind(e.PeerKind) {
				background = true
			}
		}
		if e.Role == "peer" || e.Role == "child" || background && e.Role == "agent" && !forHuman(e.Text) {
			work.add(e)
			continue
		}
		flush(false)
		// Dispatch receipts and tool-only responses follow their agent message
		// without an extra blank line or another agent marker.
		callOnly := e.Text == "" && len(e.Calls) > 0
		if !callOnly || lastRole != "agent" {
			gap()
		}
		show := e.Role != lastRole
		var content []bodyLine
		if !callOnly {
			if u.cache != nil {
				content = u.cache.get(e, l.text, u.Theme)
			} else {
				content = entryBody(e, l.text, u.Theme)
			}
		}
		for i, b := range content {
			mark := b.seam
			switch e.Role {
			case "you":
				mark = u.Theme.symbol("▎", "|")
				b.text = append(line(nil), b.text...)
				b.text = joined(b.text, textLine(strings.Repeat(" ", max(0, l.text-b.text.width())), normal))
				for j := range b.text {
					b.text[j].style.bg = youBlock
					if b.text[j].style.fg == "" {
						b.text[j].style.fg = colorDefault
					}
				}
			case "agent":
				if show && i == 0 {
					mark = u.Theme.symbol("●", "*")
				}
			case "error":
				if i == 0 {
					mark = u.Theme.symbol("✗", "x")
				}
			case "host":
				if i == 0 {
					mark = u.Theme.symbol("·", ".")
				}
			}
			out = append(out, displayRow{content: b.text, role: e.Role, seam: mark})
		}
		if len(e.Calls) > 0 {
			var chips []bodyLine
			timed := false
			if u.cache != nil {
				chips, timed = u.cache.chipRows(e, s, u, l)
			} else {
				chips = toolChips(e, s, u, l)
			}
			start := len(out)
			for _, b := range chips {
				out = append(out, displayRow{content: b.text})
			}
			if timed && u.collect != nil {
				u.collect.patches = append(u.collect.patches, timedChips{start, len(out), e})
			}
		}
		lastRole = e.Role
		if e.Role == "you" {
			gap()
		}
	}
	flush(backgroundTurn(s) && mainBusy(s) && s.Connected && s.Running && u.Transcript == nil)
	if u.Transcript == nil && s.Connected && s.Running && s.Progress != nil && s.Progress.Mode == "streaming" && s.Progress.Text != "" {
		gap()
		for _, b := range entryBody(Entry{Role: "draft", Text: s.Progress.Text}, l.text, u.Theme) {
			out = append(out, displayRow{content: b.text, role: "draft", seam: u.Theme.symbol("⋮", ":")})
		}
	}
	if len(out) == 0 && !s.Connected {
		out = append(out, displayRow{content: textLine("connecting to the host", meta)})
	}
	return out
}

func composer(s Snapshot, u UIState, l layout) ([]line, int, int, int, string) {
	label := "you"
	command := strings.HasPrefix(u.Input, "/") && !u.Pasted
	if command {
		label = "cmd"
	}
	input, cursor := u.Input, min(max(0, u.Cursor), len(u.Input))
	if u.Secret {
		maskedCursor := min(64, uniseg.GraphemeClusterCount(input[:cursor]))
		input = strings.Repeat("*", min(64, uniseg.GraphemeClusterCount(input)))
		cursor = maskedCursor
	}
	prompt := textLine(u.Theme.symbol("›", ">")+" ", visualStyle(stateYou))
	if label == "cmd" {
		prompt = joined(u.Theme.roleLine(" "+label+" ", chipColor), textLine(" ", normal), prompt)
	}
	textWidth := min(l.text, max(1, l.main-prompt.width()))
	placeholder := ""
	if input == "" {
		placeholder = "Ask, or type / for commands"
		if child := focus(u, s); child != "" {
			placeholder = "Message " + string(s.ID) + ", or /child-send TEXT for " + string(child)
		}
		switch {
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
		if str != "\n" && col+w > textWidth {
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
			if w > textWidth {
				str = "�"
				w = 1
			}
			color := normal
			if u.Selected {
				color.reverse = true
			}
			current = append(current, textLine(str, color)...)
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
	if cursorCol >= textWidth {
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
		parts[0] = textLine(placeholder, meta).clip(textWidth)
	}
	rows := make([]line, 0, end-start)
	for i, p := range parts[start:end] {
		prefix := textLine(strings.Repeat(" ", prompt.width()), normal)
		if i == 0 {
			prefix = prompt
		}
		rows = append(rows, joined(prefix, p).clip(l.main))
	}
	return rows, cursorRow - start, min(l.main-1, prompt.width()+cursorCol), len(parts) - (end - start), label
}

func rule(u UIState, l layout, overflow int, label string) line {
	value := ""
	switch {
	case u.Selected:
		value = "Type/paste replace   Backspace/Delete erase   Esc/arrows deselect"
	case u.Picker != nil:
		value = "selection; Esc back/cancel"
		if u.Picker.Kind == "file" {
			value = "files; Enter/Tab inserts path; Esc closes"
		}
	case u.Sheet != nil && u.Sheet.Analysis != "":
		value = "PgUp/PgDn scroll  Esc/Enter back  ^D detach"
	case u.Pasted:
		value = "pasted: Enter submits; registered commands run"
	case label == "cmd":
		value = "commands"
	case u.View.effective(l.width+1) == ViewOrchestration:
		value = fmt.Sprintf("Orchestration: line %d; PgUp/PgDn scroll", u.OrchestrationScroll+1)
	case u.Scroll > 0 || u.Transcript != nil:
		value = fmt.Sprintf("scrollback: %d lines below; PgDn returns", u.Scroll)
		if u.Transcript != nil && u.Transcript.Newer {
			value = fmt.Sprintf("scrollback: %d lines below in page; PgDn continues", u.Scroll)
		}
	case u.View == ViewSplit && l.width < 119:
		value = "Chat; Split returns at 120 columns"
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
	mark := u.Theme.symbol("─", "-")
	if value == "" {
		return textLine(strings.Repeat(mark, l.width), meta)
	}
	value = tail(value, max(0, l.width-4), u.Theme)
	return textLine(mark+" "+value+" "+strings.Repeat(mark, max(0, l.width-uniseg.StringWidth(value)-3)), meta).clip(l.width)
}
func keybar(s Snapshot, u UIState, l layout, label string) line {
	enter, tab := u.Theme.symbol("⏎", "enter"), u.Theme.symbol("⇥", "tab")
	value := enter + " send   ^A select all   / commands   ^C stop session   ^D detach"
	switch {
	case u.Selected && u.Picker == nil && u.Sheet == nil:
		value = "Type/paste replace  Backspace/Delete erase  Esc/arrows deselect"
	case u.Picker != nil:
		value = u.Theme.symbol("↑↓", "up/down") + " select  " + enter + " continue  Esc back/cancel  ^D detach"
		if u.Picker.Kind == "response-export" {
			value = u.Theme.symbol("↑↓", "up/down") + " select  " + enter + " export  Esc cancel  ^D detach"
		}
		if u.Picker.Kind == "view" {
			value = u.Theme.symbol("↑↓", "up/down") + " select  " + enter + " apply  Esc cancel  ^D detach"
		}
		if u.Picker.Kind == "file" {
			value = u.Theme.symbol("↑↓", "up/down") + " select  Enter/Tab insert  Esc close  ^D detach"
		}
	case u.Sheet != nil && u.Sheet.Analysis != "":
		value = "PgUp/PgDn scroll  Esc/Enter back  ^D detach"
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
		value = enter + " send  ^A all  / cmds  ^C stop  ^D detach"
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
	if u.cache != nil {
		if f, ok := u.cache.inputFrame(s, u, width, height); ok {
			return f
		}
		u.index = u.cache.prepare(s, u)
	} else {
		var temporary bodyCache
		u.index = temporary.prepare(s, u)
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
		f.Lines = append(f.Lines, header(s, u, l).paint(u.Theme))
		if u.Picker != nil {
			rows, offset := pickerRows(u.Picker, u, l, max(0, l.height-3))
			f.PickerOffset = offset
			for _, row := range rows {
				f.Lines = append(f.Lines, row.clip(l.width).paint(u.Theme))
			}
		}
		if l.height > 1 {
			for len(f.Lines) < l.height-2 {
				f.Lines = append(f.Lines, "")
			}
			text := u.Input
			if u.Secret {
				text = strings.Repeat("*", min(64, uniseg.GraphemeClusterCount(text)))
			}
			color := normal
			if u.Selected {
				color.reverse = true
			}
			f.Lines = append(f.Lines, textLine(text, color).clip(l.width).paint(u.Theme))
			f.CursorX = min(max(0, l.width-1), uniseg.StringWidth(text))
			f.CursorY = len(f.Lines) - 1
		}
		f.Lines = append(f.Lines, statisticsBar(s, u, l).paint(u.Theme))
		if len(f.Lines) > l.height {
			drop := len(f.Lines) - l.height
			f.Lines = f.Lines[drop:]
			f.CursorY = max(0, f.CursorY-drop)
		}
		return f
	}
	inputLayout := l
	shortHint := ""
	if height < 16 {
		shortHint = "^C stop"
		if u.Busy {
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
		l.text = min(100, max(1, l.main-2))
	}
	// Chat is the conversation alone; children show in its one-line dock.
	// The work pane belongs to Split.
	var attention []line
	if u.cache != nil {
		attention = u.cache.attentionRows(s, u, full)
	} else {
		attention = attentionBand(s, u, full)
	}
	footer := len(attention) + 2 + len(input) // rule, composer, permanent statistics row
	if l.height >= 16 {
		footer++
	}
	dockUI := u
	if f.View == ViewOrchestration && dockUI.Busy {
		dockUI.Outbox = "request pending"
	}
	states := stateDock(s, dockUI, l)
	notifications := notificationDock(dockUI, l)
	blank := 0
	if l.height >= 20 {
		blank = 1
	}
	minimum := 3
	if l.height >= 20 {
		minimum = l.height * 2 / 5
	}
	if u.Picker != nil && u.Picker.Kind == "permission-once" {
		// Permission scopes take priority over old conversation and dock rows.
		minimum = 1
		states = nil
		notifications = nil
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
	f.ConversationY, f.ConversationWidth = 1+blank, l.main
	var body []displayRow
	if f.View != ViewOrchestration {
		if u.cache != nil {
			body = u.cache.conversationRows(s, u, l)
		} else {
			body = conversation(s, u, l)
		}
	}
	f.ConversationRows = len(body)
	f.ScrollMax = max(0, len(body)-conversationRoom)
	scroll := min(max(0, u.Scroll), f.ScrollMax)
	end := max(0, len(body)-scroll)
	start := max(0, end-conversationRoom)
	visible := body[start:end]
	var headerRow line
	if u.cache != nil {
		headerRow = u.cache.header(s, u, l)
	} else {
		headerRow = header(s, u, l)
	}
	rows := []line{headerRow}
	if blank > 0 {
		rows = append(rows, nil)
	}
	var topology OrchestrationSnapshot
	if f.View != ViewChat {
		if u.cache != nil {
			topology.cards, topology.beads = u.cache.workCards(s, u)
		} else {
			topology.cards, topology.beads = buildWorkCards(s, u.Viewer, u.Now)
		}
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
	rows = append(rows, attention...)
	if l.height >= 16 {
		rows = append(rows, keybar(s, u, full, label))
	}
	rows = append(rows, rule(u, full, overflow, label))
	f.CursorY = len(rows) + cy
	f.CursorX = max(0, cx)
	rows = append(rows, input...)
	if u.cache != nil {
		rows = append(rows, u.cache.statistics(s, u, full))
	} else {
		rows = append(rows, statisticsBar(s, u, full))
	}
	for _, r := range rows[:min(len(rows), l.height)] {
		f.Lines = append(f.Lines, r.clip(l.width).paint(u.Theme))
	}
	f.CursorY = min(f.CursorY, l.height-1)
	if u.cache != nil {
		u.cache.rememberFrame(s, u, width, height, f, f.CursorY-cy, len(input), overflow)
	}
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

// forHuman reports a message the agent marked for the user.
func forHuman(text string) bool {
	text = strings.TrimSpace(text)
	return displaytext.ForHuman(text)
}
