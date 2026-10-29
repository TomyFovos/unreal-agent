package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

// Cards contain display metadata only. BuildOrchestration's bounded operation
// nodes stay available, while marks and completion beads cover every child.
type workCard struct {
	id                      session.ID
	mark, title, now, model string
	elapsed                 string
	state                   visualState
	depth                   int
	main, stalled, observed bool
	activity                [10]int // -1 is before creation; zero is observed inactivity
}

type workBead struct {
	id    session.ID
	state visualState
	at    time.Time
}

func safeCardText(value string) string {
	return topologyText(value, len(value)*2)
}

func childTitle(r viewer.Row) string {
	return safeCardText(strings.SplitN(r.Label, "\n", 2)[0])
}

func childStalled(r viewer.Row, s Snapshot, now time.Time) bool {
	return childVisualState(r, s) == stateWorking && !r.LastActivityAt.IsZero() && now.Sub(r.LastActivityAt) >= 5*time.Minute
}

func cardModel(r viewer.Row) string {
	if r.Selection == nil {
		return ""
	}
	value := r.Selection.Model
	if r.Selection.Effort != "" {
		value += " · " + string(r.Selection.Effort)
	}
	return safeCardText(value)
}

func currentChildOperation(r viewer.Row) string {
	if !observed(r) {
		return ""
	}
	var latest *viewer.OperationRow
	for i := range r.Operations {
		op := &r.Operations[i]
		if !operationVisualState(op.Status).ended() && (latest == nil || op.Sequence > latest.Sequence) {
			latest = op
		}
	}
	if latest != nil {
		name, target := displayTool(latest.Tool, latest.Target)
		return safeCardText(strings.TrimSpace(name + " " + target))
	}
	activity := strings.TrimPrefix(r.Activity, "tool: ")
	if strings.HasPrefix(activity, "input: ") {
		activity = "受信: " + strings.TrimPrefix(activity, "input: ")
	}
	return safeCardText(activity)
}

// Use ten consecutive one-minute buckets ending now, matching viewer's rolling
// ten-minute history. A bucket overlapping creation can contain real activity.
func activityBuckets(r viewer.Row, now time.Time) (bins [10]int) {
	start := now.Add(-10 * time.Minute)
	for i := range bins {
		end := start.Add(time.Duration(i+1) * time.Minute)
		if !r.CreatedAt.IsZero() && (end.Before(r.CreatedAt) || i < 9 && end.Equal(r.CreatedAt)) {
			bins[i] = -1
		}
	}
	for _, at := range r.RecentActivity {
		if at.Before(start) || at.After(now) || !r.CreatedAt.IsZero() && at.Before(r.CreatedAt) {
			continue
		}
		i := min(9, int(at.Sub(start)/time.Minute))
		if bins[i] >= 0 {
			bins[i]++
		}
	}
	return bins
}

func activityBars(card workCard, t Theme) line {
	if !card.observed {
		return nil
	}
	var out line
	role := stateRun
	if card.main {
		role = stateMain
	}
	for _, count := range card.activity {
		mark, color := " ", dimColor
		switch {
		case count >= 8:
			mark, color = "▇", role
		case count >= 5:
			mark, color = "▅", role
		case count >= 3:
			mark, color = "▃", role
		case count == 2:
			mark, color = "▂", role
		case count == 1:
			mark, color = "▁", role
		case count == 0:
			mark = t.symbol("─", "-")
		}
		out = append(out, textLine(t.chrome(mark), visualStyle(color))...)
	}
	return out
}

type cardTiming struct {
	value          time.Duration
	known, running bool
}
type workCardIndex struct {
	cards   []workCard
	beads   []workBead
	rows    []viewer.Row
	timings []cardTiming
	main    viewer.Row
	source  Snapshot
	at      time.Time
}

func buildWorkCards(s Snapshot, panel viewer.PanelSnapshot, now time.Time) ([]workCard, []workBead) {
	index := newWorkCardIndex(s, panel, now)
	return index.cards, index.beads
}

// Ordering, retention, safe strings and completion beads depend on data, not
// the clock. Keep them until the source projection changes.
func newWorkCardIndex(s Snapshot, panel viewer.PanelSnapshot, now time.Time) *workCardIndex {
	u := UIState{Viewer: panel, Now: now}
	rows := children(u, s)
	marks := childMarks(rows)
	byID := make(map[session.ID]viewer.Row, len(rows))
	states := make(map[session.ID]visualState, len(rows))
	activeDescendant := make(map[session.ID]bool)
	lastUser := s.LastUserAt
	for _, e := range s.Entries {
		if e.Role == "you" && e.At.After(lastUser) {
			lastUser = e.At
		}
	}
	for _, r := range rows {
		byID[r.ID], states[r.ID] = r, childVisualState(r, s)
	}
	for _, r := range rows {
		if state := states[r.ID]; state != stateWorking && state != stateCanceling {
			continue
		}
		seen := map[session.ID]bool{r.ID: true}
		for id := r.ParentID; id != "" && !seen[id]; id = byID[id].ParentID {
			seen[id], activeDescendant[id] = true, true
		}
	}
	var main viewer.Row
	for _, r := range panel.Rows {
		if r.ID == s.ID {
			main = r
			break
		}
	}
	if main.Selection == nil {
		main.Selection = s.Selection
	}
	cards := []workCard{{id: s.ID, title: "◆ メイン", main: true, model: cardModel(main), now: mainCardPulse(s, main, rows, now), observed: observed(main), activity: activityBuckets(main, now)}}
	if main.Elapsed.Known {
		cards[0].elapsed = coarseDuration(main.Elapsed.Value)
	}
	slices.SortFunc(rows, func(a, b viewer.Row) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	lineage := map[session.ID][]viewer.Row{}
	retained := map[session.ID]bool{}
	var beads []workBead
	for _, r := range rows {
		state := states[r.ID]
		keep := !state.ended() || state == stateFailed && (activeDescendant[r.ID] || r.Finish != nil && r.Finish.RecordedAt.After(lastUser))
		retained[r.ID] = keep
		if keep {
			lineage[r.ParentID] = append(lineage[r.ParentID], r)
		} else {
			// Activity after a finish is not a completion timestamp. Without
			// Finish, keep the time unknown and use ID only as a stable tie-break.
			var at time.Time
			if r.Finish != nil {
				at = r.Finish.RecordedAt
			}
			beads = append(beads, workBead{id: r.ID, state: state, at: at})
		}
	}
	cardRows := []viewer.Row{main}
	seen := map[session.ID]bool{}
	var walk func(viewer.Row, int)
	walk = func(r viewer.Row, depth int) {
		if seen[r.ID] {
			return
		}
		seen[r.ID] = true
		depth = max(depth, max(0, r.Depth-1))
		card := workCard{id: r.ID, mark: marks[r.ID], title: childTitle(r), state: states[r.ID], depth: depth, elapsed: childElapsed(r, s, u), model: cardModel(r), now: currentChildOperation(r), observed: observed(r), activity: activityBuckets(r, now)}
		if childStalled(r, s, now) {
			card.stalled = true
			card.now = fmt.Sprintf("動きなし %d分", int(now.Sub(r.LastActivityAt)/time.Minute))
		}
		cards = append(cards, card)
		cardRows = append(cardRows, r)
		for _, child := range lineage[r.ID] {
			walk(child, depth+1)
		}
	}
	// A retained ancestor stays immediately above its descendants, including a
	// failed ancestor. Independent failures follow the working lineages.
	for _, failed := range []bool{false, true} {
		for _, r := range rows {
			if !retained[r.ID] || retained[r.ParentID] || (states[r.ID] == stateFailed && !activeDescendant[r.ID]) != failed {
				continue
			}
			walk(r, 0)
		}
	}
	// Handle incomplete/cyclic observation without inventing a parent edge.
	for _, r := range rows {
		if retained[r.ID] && !seen[r.ID] {
			walk(r, 0)
		}
	}
	slices.SortFunc(beads, func(a, b workBead) int {
		if n := a.at.Compare(b.at); n != 0 {
			return n
		}
		return strings.Compare(string(a.id), string(b.id))
	})
	index := &workCardIndex{cards: cards, beads: beads, rows: cardRows, main: main, source: s, at: now}
	index.source.Operations = nil
	for _, op := range s.Operations {
		if !operationVisualState(op.Status).ended() && op.ToolName != "SubagentStart" {
			index.source.Operations = append(index.source.Operations, op)
		}
	}
	for _, r := range cardRows {
		timing := cardTiming{value: r.Elapsed.Value, known: r.Elapsed.Known, running: r.Finish == nil && r.Runtime == viewer.RuntimeRunning}
		if !s.Connected && r.Finish == nil {
			timing.known = false
		}
		if !cards[len(index.timings)].main {
			for _, parent := range panel.Rows {
				if parent.ID != r.ParentID {
					continue
				}
				for _, op := range parent.Operations {
					if op.ID == r.ParentOperationID && op.Elapsed.Known {
						timing = cardTiming{value: op.Elapsed.Value, known: true, running: !terminalStatus(op.Status) && parent.Runtime == viewer.RuntimeRunning}
						break
					}
				}
				break
			}
		}
		index.timings = append(index.timings, timing)
	}
	return index
}

func (index *workCardIndex) snapshot(now time.Time) ([]workCard, []workBead) {
	cards := append([]workCard(nil), index.cards...)
	delta := now.Sub(index.at)
	main := index.main
	main.Operations = append([]viewer.OperationRow(nil), main.Operations...)
	for i := range main.Operations {
		if main.Runtime == viewer.RuntimeRunning && main.Operations[i].Elapsed.Known && !terminalStatus(main.Operations[i].Status) {
			main.Operations[i].Elapsed.Value += delta
		}
	}
	for i := range cards {
		c, r := &cards[i], index.rows[i]
		timing := index.timings[i]
		if timing.known {
			elapsed := timing.value
			if timing.running {
				elapsed += delta
			}
			c.elapsed = coarseDuration(elapsed)
		}
		c.activity = activityBuckets(r, now)
		if c.main {
			c.now = mainCardPulse(index.source, main, index.rows[1:], now)
		} else if childStalled(r, index.source, now) {
			c.stalled = true
			c.now = fmt.Sprintf("動きなし %d分", int(now.Sub(r.LastActivityAt)/time.Minute))
		}
	}
	return cards, index.beads
}

// This pulse is local to cards; chat pulse/stateDock migrate in later steps.
func mainCardPulse(s Snapshot, main viewer.Row, rows []viewer.Row, now time.Time) string {
	if !s.Connected || main.NeedsResync {
		return "不明"
	}
	if !s.Running {
		return "停止"
	}
	phase := ""
	elapsed, known := time.Duration(0), false
	if p := s.Progress; p != nil {
		switch {
		case p.Mode == "compacting":
			phase = "会話を圧縮中"
		case p.Attempt >= 2 && p.Text == "":
			phase = fmt.Sprintf("再試行中（%d回目）", p.Attempt)
		case p.Mode == "streaming" && p.Text != "":
			phase = "生成中"
			if p.Attempt >= 2 {
				phase += fmt.Sprintf("（%d回目）", p.Attempt)
			}
		case p.Mode == "streaming":
			phase = "考え中"
		default:
			phase = "応答待ち"
		}
	} else {
		var tools []string
		for _, op := range s.Operations {
			if operationVisualState(op.Status).ended() || op.ToolName == "SubagentStart" {
				continue
			}
			target := ""
			for _, r := range main.Operations {
				if r.ID == op.ID {
					target = r.Target
					if r.Elapsed.Known && (!known || r.Elapsed.Value > elapsed) {
						known, elapsed = true, r.Elapsed.Value
					}
				}
			}
			if target == "" {
				for _, e := range s.Entries {
					for _, receipt := range e.Calls {
						if slices.Contains(receipt.Operations, op.ID) {
							target = receipt.Target
						}
					}
				}
			}
			name, _ := displayTool(op.ToolName, target)
			tools = append(tools, safeCardText(name))
		}
		switch {
		case len(tools) == 1:
			phase = tools[0] + " 実行中"
		case len(tools) > 1:
			phase = fmt.Sprintf("ツール%d件 実行中", len(tools))
		case s.WaitingForModel:
			phase = "考え中"
		default:
			n := 0
			for _, r := range rows {
				if state := childVisualState(r, s); state == stateWorking || state == stateCanceling {
					n++
				}
			}
			if n > 0 {
				return fmt.Sprintf("待機中（子 %d件 作業中）", n)
			}
		}
		if len(tools) > 0 {
			if known {
				phase += " " + duration(elapsed)
			}
			return phase
		}
	}
	if phase != "" && !s.LatestAt.IsZero() && !now.Before(s.LatestAt) {
		phase += " " + duration(now.Sub(s.LatestAt))
	}
	return phase
}

func cardColorStyle(c workCard) style {
	if c.main {
		return visualStyle(stateMain)
	}
	if c.stalled {
		return visualStyle(stateStall)
	}
	_, _, color := c.state.presentation(Theme{})
	return color
}

func workCardRows(c workCard, width int, t Theme, alternate bool) []line {
	indent := min(max(0, c.depth*2), max(0, width-1))
	room := max(0, width-indent-1)
	bg := cardColor
	if alternate {
		bg = cardAltColor
	}
	base := visualStyle(bg)
	stripe := cardColorStyle(c)
	stripe.bg = bg
	bar := textLine(t.symbol("▌", "|"), stripe)
	prefix := joined(textLine(strings.Repeat(" ", indent), normal), bar)
	finish := func(left, right line) line {
		if right.width() > room {
			right = right.clip(room)
		}
		left = left.clip(max(0, room-right.width()))
		body := joined(left, textLine(strings.Repeat(" ", max(0, room-left.width()-right.width())), base), right)
		for i := range body {
			if body[i].style.fg == "" {
				body[i].style.fg = colorDefault
			}
			if body[i].style.bg == "" {
				body[i].style.bg = bg
			}
		}
		return joined(prefix, body).clip(width)
	}
	badge := line(nil)
	if !c.main {
		badge = textLine("["+c.mark+"]", visualStyle(badgeColor))
	}
	right := textLine(c.elapsed, visualStyle(dimColor))
	if c.state == stateFailed && !c.main {
		mark, word, st := c.state.presentation(t)
		right = textLine(mark+" "+word, st)
	}
	// A child's model and effort sit before its elapsed time, so every
	// view shows what each child runs on.
	if c.model != "" && !c.main {
		model := strings.ReplaceAll(c.model, " · ", t.symbol("·", "/"))
		if right.width() > 0 {
			model += " "
		}
		right = joined(textLine(model, visualStyle(dimColor)), right)
	}
	if right.width() > 0 {
		right = joined(textLine(" ", base), right)
	}
	title := c.title
	if c.main {
		title = t.chrome(title)
	}
	left := joined(badge, textLine(" "+tail(title, max(0, room-badge.width()-1-right.width()), t), normal))
	if c.main {
		left = textLine(tail(title, max(0, room-right.width()), t), normal)
	}
	out := []line{finish(left, right)}
	activity := activityBars(c, t)
	if len(activity) > 0 {
		activity = joined(textLine(" ", base), activity)
	}
	currentStyle := visualStyle(dimColor)
	value := c.now
	if c.stalled {
		currentStyle = visualStyle(stateStall)
	} else if !c.main && c.state != stateWorking {
		mark, word, color := c.state.presentation(t)
		currentStyle = color
		if c.state != stateFailed {
			value = mark + " " + word
		}
	}
	out = append(out, finish(textLine("    "+tail(value, max(0, room-4-activity.width()), t), currentStyle), activity))
	return out
}

func finishedBeads(beads []workBead, width int, t Theme) line {
	if len(beads) == 0 {
		return nil
	}
	prefix := textLine("終了 ", visualStyle(dimColor))
	// Choose the largest suffix of recent completions that fits together with
	// the exact remaining count. Rightmost is always the newest completion.
	count := min(len(beads), max(0, width-prefix.width()))
	for count >= 0 {
		hidden := len(beads) - count
		suffix := ""
		if hidden > 0 {
			suffix = fmt.Sprintf("  ほか %d", hidden)
		}
		if prefix.width()+count+textLine(suffix, normal).width() <= width || count == 0 {
			out := append(line(nil), prefix...)
			for _, bead := range beads[len(beads)-count:] {
				mark, _, st := bead.state.presentation(t)
				out = append(out, textLine(mark, st)...)
			}
			return joined(out, textLine(suffix, visualStyle(dimColor))).clip(width)
		}
		count--
	}
	return nil
}

func workCardPane(snapshot OrchestrationSnapshot, u UIState, width, height int, compact bool) ([]line, int, int) {
	if height <= 0 {
		return nil, 0, 0
	}
	cards := snapshot.cards
	beads := finishedBeads(snapshot.beads, width, u.Theme)
	reserved := 0
	if len(beads) > 0 {
		reserved = 1
	}
	const block = 2
	spaced := len(cards)*block+max(0, len(cards)-1)+reserved <= height
	if compact && len(cards)*block+reserved > height {
		count := min(len(cards), max(0, height-reserved-1)/block)
		var rows []line
		for i, c := range cards[:count] {
			rows = append(rows, cardRows(c, width, u, i%2 == 1)...)
		}
		if reserved > 0 && len(rows) < height {
			rows = append(rows, beads)
		}
		if len(rows) < height {
			text := fmt.Sprintf("＋%d 件  /view orchestration", len(cards)-count)
			if u.Theme.ASCII {
				text = strings.Replace(text, "＋", "+", 1)
			}
			rows = append(rows, textLine(tail(text, width, u.Theme), visualStyle(dimColor)))
		}
		return rows, 0, max(0, len(cards)*2+reserved-height)
	}
	var rows []line
	for i, c := range cards {
		if spaced && i > 0 {
			rows = append(rows, nil)
		}
		rows = append(rows, cardRows(c, width, u, !spaced && i%2 == 1)...)
	}
	if !compact {
		if len(beads) > 0 {
			rows = append(rows, beads)
		}
		maxOffset := max(0, len(rows)-height)
		start := min(max(0, u.OrchestrationScroll), maxOffset)
		return rows[start:min(len(rows), start+height)], start, maxOffset
	}
	if reserved > 0 {
		rows = append(rows, beads)
	}
	return rows, 0, 0
}

// Protect focus-sheet labels too; operation targets and titles are never raw
// tool state or credential bindings.
func protectedCardText(value string) string {
	if secretguard.Sensitive(value) {
		return "[protected]"
	}
	return SafeText(value)
}
