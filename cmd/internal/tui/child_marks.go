package tui

import (
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func marksFor(u UIState, s Snapshot) map[session.ID]string {
	if u.index != nil {
		return u.index.marks
	}
	return childMarks(children(u, s))
}

// childMarks computes session-local labels from canonical creation times,
// without reordering the caller's rows or retaining state between frames.
func childMarks(rows []viewer.Row) map[session.ID]string {
	children := make([]viewer.Row, 0, len(rows))
	for _, r := range rows {
		if r.ParentID != "" {
			children = append(children, r)
		}
	}
	slices.SortFunc(children, func(a, b viewer.Row) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	marks := make(map[session.ID]string, len(children))
	for i, r := range children {
		marks[r.ID] = alphabeticMark(i)
	}
	return marks
}

func alphabeticMark(index int) string {
	var letters []byte
	for {
		letters = append(letters, byte('A'+index%26))
		index = index/26 - 1
		if index < 0 {
			break
		}
	}
	slices.Reverse(letters)
	return string(letters)
}
