package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func children(u UIState, s Snapshot) []viewer.Row {
	if u.index != nil {
		return u.index.children
	}
	var rows []viewer.Row
	parents := make(map[session.ID]viewer.Liveness, len(u.Viewer.Rows))
	parentResync := false
	for _, r := range u.Viewer.Rows {
		parents[r.ID] = r.Runtime
		if r.ID == s.ID && r.NeedsResync {
			parentResync = true
		}
	}
	for _, r := range u.Viewer.Rows {
		if r.ID != s.ID && r.ParentID != "" {
			if runtime := parents[r.ParentID]; runtime != "" {
				r.ParentRuntime = runtime
			}
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

// Legacy chat receipts migrate with the chat redesign; cards and docks use visualState.
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
	switch childVisualState(r, s) {
	case stateEnded:
		return t.symbol("▪", "#"), "ended (no Finish)", meta, true
	case stateCanceled:
		return t.symbol("⊘", "-"), "canceled", normal, true
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
					return coarseDuration(op.Elapsed.Value)
				}
			}
		}
	}
	if r.Elapsed.Known {
		return coarseDuration(r.Elapsed.Value)
	}
	return ""
}

// coarseDuration drops seconds past the first minute: cards show how long
// work has run, not a stopwatch.
func coarseDuration(d time.Duration) string {
	if d < time.Hour && d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return duration(d)
}
func focusRows(r viewer.Row, rows []viewer.Row, s Snapshot, u UIState, l layout) []line {
	state := childVisualState(r, s)
	_, word, st := state.presentation(u.Theme)
	marks := marksFor(u, s)
	identity := "[" + marks[r.ID] + "] " + childTitle(r)
	value := identity + "  " + word
	if r.Selection != nil {
		value += "  " + selectionLabel(*r.Selection, u.Theme)
	}
	if elapsed := childElapsed(r, s, u); elapsed != "" {
		value += "  " + elapsed
	}
	out := []line{l.gutter("子", u.Theme.symbol("›", "*"), textLine(tail(value, l.text, u.Theme), strong))}
	add := func(label, text string, style style) {
		out = append(out, l.gutter("", "", joined(textLine(label+"  ", meta), textLine(tail(protectedCardText(text), max(0, l.text-textLine(label, meta).width()-2), u.Theme), style))))
	}
	if page := u.Viewer.Transcript; page != nil && u.Viewer.Selected == r.ID {
		more := "履歴の末尾"
		if page.More {
			more = "続き: /child-next"
		}
		out[0] = l.gutter("子", u.Theme.symbol("›", "*"), textLine(tail(fmt.Sprintf("%s 履歴 %d件  %s", identity, len(page.Items), more), l.text, u.Theme), strong))
		for _, item := range page.Items {
			for _, entry := range entriesFor(item) {
				if entry.Text != "" {
					add(fmt.Sprint(item.Sequence), focusRole(entry.Role)+"  "+strings.Join(strings.Fields(entry.Text), " "), normal)
				}
				for _, c := range entry.Calls {
					name := c.Name
					if name == "SubagentStart" {
						name = "spawn"
					}
					name, target := displayTool(name, c.Target)
					add("操作", name+" "+target, normal)
				}
			}
		}
		return out
	}
	if observed(r) {
		if r.Activity != "" {
			add("活動", r.Activity, normal)
		}
		if len(r.Operations) > 0 {
			counts := map[string]int{}
			var active []string
			for _, op := range r.Operations {
				m, w, _ := toolVisualState(Receipt{Status: op.Status, Operations: []operation.ID{op.ID}}, s).presentation(u.Theme)
				if op.Status == operation.StatusCompleted {
					name, _ := displayTool(op.Tool, op.Target)
					counts[name]++
				} else {
					name, _ := displayTool(op.Tool, op.Target)
					value := m + " " + name + " " + w
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
			add("操作", strings.Join(parts, "  "), normal)
		}
		if value := usage(r.Usage); value != "" {
			add("使用量", value, meta)
			add("キャッシュ", strings.TrimPrefix(viewer.CacheLabel(r.LastUsage), "cache ")+" / "+strings.TrimPrefix(viewer.CacheLabel(r.Usage), "cache ")+" 累計", meta)
		}
	}
	if f := r.Finish; f != nil {
		add("終了報告", fmt.Sprintf("%s: %s  変更%d件, テスト%d件, 支障%d件", word, f.Summary, len(f.ChangedFiles), len(f.Tests), len(f.Blockers)), st)
		if len(f.Blockers) > 0 {
			add("支障", f.Blockers[0], warningStyle)
		}
	} else {
		add("終了報告", "未記録", meta)
	}
	var other []string
	for _, row := range rows {
		if row.ID != r.ID {
			m, word, _ := childVisualState(row, s).presentation(u.Theme)
			other = append(other, m+" ["+marks[row.ID]+"] "+childTitle(row)+" "+word)
		}
	}
	if len(other) > 0 {
		add("ほか", strings.Join(other, "  "), meta)
	}
	add("", "/child-send TEXT  /child-cancel  /child-history  /child ID", meta)
	return out
}
func focusRole(role string) string {
	switch role {
	case "you":
		return "あなた"
	case "agent":
		return "応答"
	case "error":
		return "エラー"
	case "host":
		return "制御"
	case "peer":
		return "子"
	default:
		return role
	}
}

func childDock(s Snapshot, u UIState, l layout, compact bool) []line {
	rows := children(u, s)
	if len(rows) == 0 || l.rail > 0 {
		return nil
	}
	if id := focus(u, s); id != "" && !compact && !l.tiny {
		limit := 7
		if l.height < 20 {
			limit = 2
		} else if l.height >= 35 {
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
	marks := marksFor(u, s)
	// Finished children leave the dock; only failures stay in view.
	rows = slices.DeleteFunc(slices.Clone(rows), func(r viewer.Row) bool {
		state := childVisualState(r, s)
		return state.ended() && state != stateFailed
	})
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	working, failed, unknown := 0, 0, 0
	for _, r := range rows {
		switch childVisualState(r, s) {
		case stateWorking, stateCanceling:
			working++
		case stateFailed:
			failed++
		case stateUnknown:
			unknown++
		}
	}
	summary := fmt.Sprintf("   作業中 %d · 失敗 %d", working, failed)
	if u.Theme.ASCII {
		summary = strings.ReplaceAll(summary, " · ", " / ")
	}
	if unknown > 0 {
		summary += fmt.Sprintf("  不明 %d", unknown)
	}
	right := textLine(summary, visualStyle(dimColor))
	prefix := textLine("子  ", strong)
	var badges line
	for i, r := range rows {
		mark, _, color := childVisualState(r, s).presentation(u.Theme)
		if childStalled(r, s, u.Now) {
			mark, color = "!", visualStyle(stateStall)
		}
		badge := joined(textLine("["+marks[r.ID]+"]", visualStyle(badgeColor)), textLine(mark, color))
		if len(badges) > 0 {
			badge = joined(textLine(" ", normal), badge)
		}
		remainder := ""
		if i+1 < len(rows) {
			remainder = fmt.Sprintf(" +%d", len(rows)-i-1)
		}
		if prefix.width()+badges.width()+badge.width()+len(remainder)+right.width() > l.main {
			badges = joined(badges, textLine(fmt.Sprintf(" +%d", len(rows)-i), visualStyle(dimColor)))
			break
		}
		badges = append(badges, badge...)
	}
	return []line{joined(prefix, badges, right).clip(l.main)}
}

func childRail(s Snapshot, u UIState, l layout, height int) []line {
	if len(children(u, s)) == 0 || height <= 0 {
		return nil
	}
	var detail []line
	if id := focus(u, s); id != "" {
		for _, r := range children(u, s) {
			if r.ID == id {
				small := layout{width: l.rail, main: l.rail, text: l.rail - 2, height: l.height, tiny: true}
				detail = focusRows(r, children(u, s), s, u, small)
				detail = detail[:min(len(detail), min(10, height/2))]
				break
			}
		}
	}
	var snapshot OrchestrationSnapshot
	if u.cache != nil {
		snapshot.cards, snapshot.beads = u.cache.workCards(s, u)
	} else {
		snapshot.cards, snapshot.beads = buildWorkCards(s, u.Viewer, u.Now)
	}
	out, _, _ := workCardPane(snapshot, u, l.rail, height-len(detail), true)
	return append(out, detail...)
}
