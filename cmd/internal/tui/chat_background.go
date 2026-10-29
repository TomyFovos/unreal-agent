package tui

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// backgroundWork folds a contiguous run without changing canonical entries.
// Only model TurnIDs count as turns; receipts count once per displayed call.
// Report marks identify child senders, rather than newly dispatched children.
type backgroundWork struct {
	skipThrough    sessionstore.Sequence
	entries, tools int
	turns          map[session.TurnID]bool
	reports        map[session.ID]bool
}

func backgroundPrefix(prefix *host.HistoryWork) backgroundWork {
	if prefix == nil {
		return backgroundWork{}
	}
	b := backgroundWork{entries: prefix.Entries, tools: prefix.Tools, skipThrough: prefix.Through, turns: map[session.TurnID]bool{}, reports: map[session.ID]bool{}}
	for _, id := range prefix.Turns {
		b.turns[id] = true
	}
	for _, id := range prefix.Reports {
		b.reports[session.ID(SafeText(string(id)))] = true
	}
	return b
}
func (b *backgroundWork) add(e Entry) {
	if e.Sequence > 0 && e.Sequence <= b.skipThrough {
		return
	}
	if b.turns == nil {
		b.turns = make(map[session.TurnID]bool)
		b.reports = make(map[session.ID]bool)
	}
	b.entries++
	b.tools += len(e.Calls)
	if e.TurnID != "" {
		b.turns[e.TurnID] = true
	}
	// "ready" only says a child started; it is not a report.
	if e.PeerID != "" && e.PeerKind != "ready" {
		b.reports[session.ID(e.PeerID)] = true
	}
}

func (b backgroundWork) row(marks map[session.ID]string, width int, t Theme) line {
	return b.rowWithMark(marks, width, t, false, 0)
}

func (b backgroundWork) rowWithMark(marks map[session.ID]string, width int, t Theme, active bool, now int64) line {
	color := visualStyle(dimColor)
	mark := t.symbol("┄", "-")
	if active {
		mark = t.spinner(now)
	}
	row := textLine(fmt.Sprintf("%s 裏の作業  %dターン · ツール%d回", mark, len(b.turns), b.tools), color)
	var reports []string
	for id := range b.reports {
		if mark := marks[id]; mark != "" {
			reports = append(reports, mark)
		}
	}
	slices.SortFunc(reports, func(a, b string) int {
		if n := cmp.Compare(len(a), len(b)); n != 0 {
			return n
		}
		return cmp.Compare(a, b)
	})
	if len(reports) > 0 {
		reportRow := func(count int) line {
			result := joined(append(line(nil), row...), textLine(" · 報告 ", color))
			for _, mark := range reports[:count] {
				result = joined(result, t.roleLine("["+mark+"]", badgeColor))
			}
			if count < len(reports) {
				result = joined(result, textLine(fmt.Sprintf("+%d", len(reports)-count), color))
			}
			return result
		}
		count := min(8, len(reports))
		candidate := reportRow(count)
		for candidate.width() > width && count > 0 {
			count--
			candidate = reportRow(count)
		}
		if candidate.width() <= width {
			return candidate
		}
		// At very narrow widths keep the overflow count instead of splitting
		// a badge or implying that a partially visible mark is complete.
		more := textLine(fmt.Sprintf(" +%d", len(reports)), color)
		return joined(textLine(tail(row.plain(), max(0, width-more.width()), t), color), more).clip(width)
	}
	if row.width() > width {
		mark := textLine(t.symbol("…", "~"), color)
		row = joined(row.clip(max(0, width-mark.width())), mark).clip(width)
	}
	return row
}
