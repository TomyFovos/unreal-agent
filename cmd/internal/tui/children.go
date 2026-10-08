package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func children(u UIState, s Snapshot) []viewer.Row {
	var rows []viewer.Row
	parentResync := false
	for _, r := range u.Viewer.Rows {
		if r.ID == s.ID && r.NeedsResync {
			parentResync = true
		}
	}
	for _, r := range u.Viewer.Rows {
		if r.ID != s.ID && r.ParentID != "" {
			if parentResync {
				r.NeedsResync = true
			}
			rows = append(rows, r)
		}
	}
	return rows // Preserve viewer lineage/ID ordering through every state update.
}
func focus(u UIState, s Snapshot) session.ID {
	if u.Viewer.Selected == s.ID {
		return ""
	}
	for _, r := range children(u, s) {
		if r.ID == u.Viewer.Selected {
			return r.ID
		}
	}
	return ""
}
func observed(r viewer.Row) bool { return r.Generation != "" || r.Cursor > 0 }
func childState(r viewer.Row, s Snapshot, t Theme) (string, string, style, bool) {
	if f := r.Finish; f != nil {
		switch f.Status {

		case "success":
			return t.symbol("✓", "+"), "finished: success", successStyle, true
		case "failure", "failed", "error":
			return t.symbol("✗", "x"), "finished: " + SafeText(f.Status), failureStyle, true
		default:
			return "!", "finished: " + SafeText(f.Status), warningStyle, true
		}
	}
	if !s.Connected || r.NeedsResync {
		return "?", "unknown", warningStyle, false
	}
	switch r.ParentOperationStatus {
	case operation.StatusCompleted:
		return t.symbol("▪", "#"), "ended (no Finish)", meta, true
	case operation.StatusFailed:
		return t.symbol("✗", "x"), "failed", failureStyle, true
	case operation.StatusCanceled:
		return t.symbol("⊘", "-"), "canceled", normal, true
	case operation.StatusReady, operation.StatusCanceling:
		return t.symbol("▸", ">"), "running", liveStyle, false
	case operation.StatusAwaiting:
		return t.symbol("◌", "."), "queued", meta, false
	}
	return "?", "unknown", warningStyle, false
}
func childElapsed(r viewer.Row, s Snapshot, u UIState) string {
	if !s.Connected && r.Finish == nil {
		return ""
	}
	for _, p := range u.Viewer.Rows {
		if p.ID == r.ParentID {
			for _, op := range p.Operations {
				if op.ID == r.ParentOperationID && op.Elapsed.Known {
					return duration(op.Elapsed.Value)
				}
			}
		}
	}
	if r.Elapsed.Known {
		return duration(r.Elapsed.Value)
	}
	return ""
}
func childCounts(rows []viewer.Row, s Snapshot, t Theme, hidden int) string {
	running, done, failed := 0, 0, 0
	for _, r := range rows {
		_, word, _, end := childState(r, s, t)
		if strings.Contains(word, "failed") || strings.Contains(word, "failure") || strings.Contains(word, "error") {
			failed++
		} else if end {
			done++
		} else {
			running++
		}
	}
	// "running" here counts active parent operations only while connected.
	activeWord := "running"
	if !s.Connected {
		activeWord = "unknown"
	}
	value := fmt.Sprintf("%d %s, %d done, %d failed", running, activeWord, done, failed)
	if hidden > 0 {
		value += fmt.Sprintf("  (%d hidden)", hidden)
	}
	return value
}
func laneIDs(rows []viewer.Row, t Theme) map[session.ID]string {
	ids := map[session.ID]string{}
	for _, r := range rows {
		w := 12
		id := SafeText(string(r.ID))
		candidate := middle(id, w, t)
		for {
			unique := true
			for _, other := range rows {
				if other.ID != r.ID && middle(SafeText(string(other.ID)), w, t) == candidate {
					unique = false
					break
				}
			}
			if unique {
				break
			}
			w++
			candidate = middle(id, w, t)
		}
		ids[r.ID] = candidate
	}
	return ids
}
func pickLanes(rows []viewer.Row, s Snapshot, u UIState, limit int) []viewer.Row {
	chosen := map[session.ID]bool{}
	if id := focus(u, s); id != "" && limit > 0 {
		chosen[id] = true
	}
	for _, r := range rows {
		_, _, _, end := childState(r, s, u.Theme)
		if len(chosen) < limit && !end {
			chosen[r.ID] = true
		}
	}
	terminal := append([]viewer.Row(nil), rows...)
	sort.SliceStable(terminal, func(i, j int) bool {
		if terminal[i].Finish == nil {
			return false
		}
		if terminal[j].Finish == nil {
			return true
		}
		return terminal[i].Finish.RecordedAt.After(terminal[j].Finish.RecordedAt)
	})
	for _, r := range terminal {
		if len(chosen) < limit {
			chosen[r.ID] = true
		}
	}
	var out []viewer.Row
	for _, r := range rows {
		if chosen[r.ID] {
			out = append(out, r)
		}
	}
	return out
}
func lane(r viewer.Row, s Snapshot, u UIState, l layout, label, id string) line {
	mark, word, st, _ := childState(r, s, u.Theme)
	selected := r.ID == focus(u, s)
	idStyle := normal
	if selected {
		mark = u.Theme.symbol("›", "*")
		idStyle = strong
	}
	indent := strings.Repeat(" ", max(0, r.Depth-1)*2)
	base := indent + id
	end := word
	if elapsed := childElapsed(r, s, u); elapsed != "" {
		end += "  " + elapsed
	}
	activity := ""
	if l.main >= 69 && observed(r) && r.Finish == nil {
		activity = r.Activity
	}
	if activity != "" {
		end += "  " + activity
	}
	if r.NeedsResync || r.Problem != "" {
		end += "  ! resync required"
	}
	room := max(0, l.text-textLine(base, normal).width()-textLine(end, normal).width()-4)
	labelText := r.Label
	if r.Selection != nil {
		labelText = r.Selection.Provider + " / " + r.Selection.Model + " / " + string(r.Selection.Effort)
	}
	task := middle(labelText, room, u.Theme)
	if strings.Contains(id, u.Theme.symbol("…", "~")) {
		task = textLine(labelText, normal).clip(room).plain()
	}
	left := joined(textLine(base, idStyle), textLine("  "+task, normal))
	suffix := textLine("  "+end, meta)
	if strings.Contains(end, "unknown") || strings.Contains(end, "failed") || r.NeedsResync {
		suffix = textLine("  "+end, st)
	}
	if left.width()+suffix.width() > l.text {
		suffix = suffix.clip(max(0, l.text-left.width()))
	}
	return l.gutter(label, mark, joined(left, textLine(strings.Repeat(" ", max(0, l.text-left.width()-suffix.width())), normal), suffix))
}

func focusRows(r viewer.Row, rows []viewer.Row, s Snapshot, u UIState, l layout) []line {
	_, state, st, _ := childState(r, s, u.Theme)
	value := SafeText(string(r.ID)) + "  " + state
	if r.Selection != nil {
		value += "  " + selectionLabel(*r.Selection, u.Theme)
	}
	if elapsed := childElapsed(r, s, u); elapsed != "" {
		value += "  " + elapsed
	}
	out := []line{l.gutter("child", u.Theme.symbol("›", "*"), textLine(middle(value, l.text, u.Theme), strong))}
	add := func(label, text string, style style) {
		out = append(out, l.gutter("", "", joined(textLine(label+"  ", meta), textLine(middle(text, max(0, l.text-len(label)-2), u.Theme), style))))
	}
	if page := u.Viewer.Transcript; page != nil && u.Viewer.Selected == r.ID {
		more := "end of history"
		if page.More {
			more = "more: /child-next"
		}
		out[0] = l.gutter("child", u.Theme.symbol("›", "*"), textLine(middle(fmt.Sprintf("%s history after %d, %d items  %s", r.ID, u.Viewer.TranscriptAfter, len(page.Items), more), l.text, u.Theme), strong))
		for _, item := range page.Items {
			for _, entry := range entriesFor(item) {
				if entry.Text != "" {
					add(fmt.Sprint(item.Sequence), entry.Role+"  "+strings.Join(strings.Fields(entry.Text), " "), normal)
				}
				for _, c := range entry.Calls {
					name := c.Name
					if name == "SubagentStart" {
						name = "spawn"
					}
					add(fmt.Sprint(item.Sequence), "tool  "+name+" "+c.Target, normal)
				}
			}
		}
		return out
	}
	if observed(r) {
		if r.Activity != "" {
			add("activity", r.Activity, normal)
		}
		if len(r.Operations) > 0 {
			counts := map[string]int{}
			var active []string
			for _, op := range r.Operations {
				m, w, _, _ := childOpState(op, u.Theme)
				if op.Status == operation.StatusCompleted {
					counts[op.Tool]++
				} else {
					value := m + " " + op.Tool + " " + w
					if op.Elapsed.Known && s.Connected {
						value += " " + duration(op.Elapsed.Value)
					}
					active = append(active, value)
				}
			}
			var keys []string
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s %s %d", u.Theme.symbol("✓", "+"), k, counts[k]))
			}
			parts = append(parts, active...)
			add("ops", strings.Join(parts, "  "), normal)
		}
		if value := usage(r.Usage); value != "" {
			add("usage", value, meta)
		}
	}
	if f := r.Finish; f != nil {
		add("finish", fmt.Sprintf("%s: %s  %d files, %d tests, %d blockers", f.Status, f.Summary, len(f.ChangedFiles), len(f.Tests), len(f.Blockers)), st)
		if len(f.Blockers) > 0 {
			add("blocker", f.Blockers[0], warningStyle)
		}
	} else {
		add("finish", "not recorded yet", meta)
	}
	var other []string
	for _, row := range rows {
		if row.ID != r.ID {
			m, state, _, _ := childState(row, s, u.Theme)
			other = append(other, m+" "+string(row.ID)+" "+state)
		}
	}
	if len(other) > 0 {
		add("others", strings.Join(other, "  "), meta)
	}
	if l.height >= 35 && r.ProjectInstructions != nil {
		p := r.ProjectInstructions
		add("project", fmt.Sprintf("%s %s  %d bytes  %s", p.SourceKind, p.SourcePath, p.ByteLength, SafeText(p.Digest)[:min(8, len(SafeText(p.Digest)))]), meta)
	}
	add("", "/child-send TEXT  /child-cancel  /child-history  /child "+string(s.ID), meta)
	return out
}
func childOpState(op viewer.OperationRow, t Theme) (string, string, style, bool) {
	m, w, s := stateMark(op.Status, t)
	return m, w, s, terminalStatus(op.Status)
}

func childDock(s Snapshot, u UIState, l layout, compact bool) []line {
	rows := children(u, s)
	if len(rows) == 0 || l.rail > 0 {
		return nil
	}
	budget := max(1, min(6, l.height/6))
	if l.tiny || compact {
		budget = 1
	}
	if id := focus(u, s); id != "" && !compact && !l.tiny {
		limit := 7
		if l.height < 20 {
			limit = 2
		}
		if l.height >= 35 {
			limit = 10
		}
		for _, r := range rows {
			if r.ID == id {
				out := focusRows(r, rows, s, u, l)
				if len(out) > limit && limit == 2 {
					return out[:2]
				}
				if len(out) > limit {
					last := out[len(out)-1]
					out = append(out[:limit-1], last)
				}
				return out
			}
		}
	}
	if len(rows) == 1 && budget > 1 || len(rows) == 1 && !l.tiny && !compact {
		ids := laneIDs(rows, u.Theme)
		return []line{lane(rows[0], s, u, l, "child", ids[rows[0].ID])}
	}
	picked := pickLanes(rows, s, u, max(0, budget-1))
	text := childCounts(rows, s, u.Theme, len(rows)-len(picked))
	if u.Viewer.Problem != "" {
		text += "  ! observation: " + u.Viewer.Problem
	}
	seam := ""
	if l.tiny && focus(u, s) != "" {
		seam = u.Theme.symbol("›", "*")
	}
	out := []line{l.gutter("child", seam, textLine(middle(text, l.text, u.Theme), meta))}
	ids := laneIDs(picked, u.Theme)
	for _, r := range picked {
		out = append(out, lane(r, s, u, l, "", ids[r.ID]))
	}
	return out
}

func childRail(s Snapshot, u UIState, l layout, height int) []line {
	rows := children(u, s)
	if len(rows) == 0 || height < 1 {
		return nil
	}
	width := l.rail
	count := fmt.Sprint(len(rows))
	title := joined(textLine("child agents", strong), textLine(strings.Repeat(" ", max(1, width-len("child agents")-len(count)))+count, meta))
	out := []line{title.clip(width)}
	ids := laneIDs(rows, u.Theme)
	selected := focus(u, s)
	focusLimit := 0
	if selected != "" {
		focusLimit = min(10, max(2, height/2))
	}
	budget := max(0, height-1-focusLimit)
	block := 3
	if len(rows)*3 > budget {
		block = 1
	}
	picked := pickLanes(rows, s, u, budget/max(1, block))
	for _, r := range picked {
		mark, state, st, _ := childState(r, s, u.Theme)
		prefix := ""
		idStyle := normal
		if r.ID == selected {
			prefix = u.Theme.symbol("›", "*") + " "
			idStyle = strong
		}
		left := joined(textLine(prefix, normal), textLine(mark+" ", st), textLine(ids[r.ID], idStyle))
		elapsed := childElapsed(r, s, u)
		rightText := elapsed
		if block == 1 && state != "running" {
			rightText = state
			if elapsed != "" && left.width()+len(state)+len(elapsed)+3 <= width {
				rightText += " " + elapsed
			}
		}
		if block == 1 && (r.NeedsResync || r.Problem != "") {
			rightText = state + " ! resync required"
		}
		right := textLine(rightText, meta)
		out = append(out, joined(left, textLine(strings.Repeat(" ", max(1, width-left.width()-right.width())), normal), right).clip(width))
		if block == 3 {
			label := r.Label
			if r.Selection != nil {
				label = r.Selection.Provider + " / " + r.Selection.Model + " / " + string(r.Selection.Effort)
			}
			out = append(out, textLine(middle(label, width, u.Theme), normal))
			value := state
			if observed(r) && r.Activity != "" && r.Finish == nil {
				value = r.Activity
			}
			out = append(out, textLine(middle(value, width, u.Theme), meta))
		}
		if block == 3 && (r.NeedsResync || r.Problem != "") {
			i := len(out) - 1
			out[i] = textLine(middle(state+"  ! resync required", width, u.Theme), warningStyle)
		}
	}
	if hidden := len(rows) - len(picked); hidden > 0 {
		if len(out) < height-focusLimit {
			out = append(out, textLine(childCounts(rows, s, u.Theme, hidden), meta).clip(width))
		} else {
			out[0] = textLine(middle(fmt.Sprintf("child agents %d (%d hidden)", len(rows), hidden), width, u.Theme), strong)
		}
	}
	if selected != "" {
		for _, r := range rows {
			if r.ID == selected {
				small := layout{width: width, main: width, text: width - 2, height: l.height, tiny: true}
				focused := focusRows(r, rows, s, u, small)
				for _, row := range focused[:min(len(focused), focusLimit)] {
					out = append(out, row.clip(width))
				}
				break
			}
		}
	}
	if u.Viewer.Problem != "" && len(out) < height {
		out = append(out, textLine(middle("! observation: "+u.Viewer.Problem, width, u.Theme), warningStyle))
	}
	return out[:min(len(out), height)]
}
