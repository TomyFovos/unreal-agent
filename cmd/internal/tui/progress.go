package tui

import (
	"fmt"
	"time"
)

// mainBusy describes observed work, independently of whether it was started
// by a person or a child notification.
func mainBusy(s Snapshot) bool {
	if s.Progress != nil || s.WaitingForModel {
		return true
	}
	for _, op := range s.Operations {
		if !terminalStatus(op.Status) && op.ToolName != "SubagentStart" {
			return true
		}
	}
	return false
}

func backgroundTurn(s Snapshot) bool {
	for i := len(s.Entries) - 1; i >= 0; i-- {
		switch s.Entries[i].Role {
		case "you":
			return false
		case "peer", "child":
			if !quietPeerKind(s.Entries[i].PeerKind) {
				return true
			}
		}
	}
	return s.TailBackground
}

func workingChildren(s Snapshot, u UIState) int {
	n := 0
	for _, r := range children(u, s) {
		if state := childVisualState(r, s); state == stateWorking || state == stateCanceling {
			n++
		}
	}
	return n
}

func backgroundActive(s Snapshot, u UIState) bool {
	return s.Connected && s.Running && runtime(s) != "failed" &&
		(backgroundTurn(s) && mainBusy(s) || !mainBusy(s) && workingChildren(s, u) > 0)
}

func pulse(s Snapshot, u UIState, l layout) line {
	if !s.Connected || !s.Running || runtime(s) == "failed" {
		return nil
	}
	phase := ""
	runningTools := false
	elapsed := time.Duration(0)
	known := false
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
			if !terminalStatus(op.Status) && op.ToolName != "SubagentStart" {
				target := ""
				if u.Parent != nil {
					for _, r := range u.Parent.Operations {
						if r.ID != op.ID {
							continue
						}
						target = r.Target
						if r.Elapsed.Known && (!known || r.Elapsed.Value > elapsed) {
							known, elapsed = true, r.Elapsed.Value
						}
					}
				}
				name, _ := displayTool(op.ToolName, target)
				if name == "" {
					name = "—"
				}
				tools = append(tools, name)
			}
		}
		if len(tools) == 1 {
			phase, runningTools = tools[0]+" 実行中", true
		} else if len(tools) > 1 {
			phase, runningTools = fmt.Sprintf("ツール%d件 実行中", len(tools)), true
		} else if s.WaitingForModel {
			phase = "考え中"
		}
	}
	if backgroundActive(s, u) && (s.Progress == nil || s.Progress.Mode != "compacting") {
		phase = backgroundLabel(workingChildren(s, u))
	}
	if phase == "" {
		return nil
	}
	if !runningTools && !s.LatestAt.IsZero() && !u.Now.Before(s.LatestAt) {
		known = true
		elapsed = u.Now.Sub(s.LatestAt)
	}
	text := textLine(phase, normal)
	if known {
		text = joined(text, textLine("  "+coarseDuration(elapsed), meta))
	}
	return l.markedGutter(u.Theme.spinner(u.Now.UnixMilli()), visualStyle(stateMain), text)
}

func stateDock(s Snapshot, u UIState, l layout) []line {
	var result []line
	conn := connection(s, u.Now)
	if !s.Connected {
		if conn == "resyncing" {
			return []line{l.gutter("", "", textLine(tail("履歴から表示を作り直しています", l.text, u.Theme), meta))}
		}
		if conn == "disconnected" {
			elapsed := ""
			if !s.OfflineSince.IsZero() {
				elapsed = "  " + duration(u.Now.Sub(s.OfflineSince))
			}
			text := "再接続中" + elapsed + " · 再接続を待っています"
			return []line{l.gutter("", u.Theme.spinner(u.Now.UnixMilli()), textLine(tail(text, l.text, u.Theme), warningStyle))}
		}
		return nil
	}
	if s.PendingSelection != nil {
		text := "次のターンから: " + topologyText(selectionLabel(*s.PendingSelection, u.Theme), 256)
		result = append(result, l.gutter("", "", textLine(tail(text, l.text, u.Theme), meta)))
	}
	return result
}

func notificationDock(u UIState, l layout) []line {
	if u.Busy && (u.Outbox != "" || u.Notification.Text == "") {
		value := u.Outbox
		if value == "" {
			value = "request pending"
		}
		out := []line{l.gutter("", u.Theme.spinner(u.Now.UnixMilli()), textLine(middle(value, l.text, u.Theme), meta))}
		if u.Notification.Text != "" {
			out = append(out, l.gutter("", "!", textLine(middle(u.Notification.Text, l.text, u.Theme), warningStyle)))
		}
		return out
	}
	n := u.Notification
	if n.Text == "" || !n.Until.IsZero() && !u.Now.Before(n.Until) {
		return nil
	}
	st, mark := meta, ""
	if n.Kind == "error" {
		st = failureStyle
		mark = u.Theme.symbol("✗", "x")
	} else if n.Kind == "warning" {
		st = warningStyle
		mark = "!"
	}
	var result []line
	if n.Hint != "" {
		return []line{l.gutter("", mark, textLine(middle(n.Text, l.text, u.Theme), st)), l.gutter("", "", textLine(n.Hint, meta).clip(l.text))}
	}
	for i, v := range wrapLine(textLine(n.Text, st), l.text, false, 0) {
		if i >= 2 {
			break
		}
		seam := ""
		if i == 0 {
			seam = mark
		}
		result = append(result, l.gutter("", seam, v))
	}
	return result
}

// backgroundLabel names background work; a background turn with no child
// running shows no count.
func backgroundLabel(children int) string {
	if children == 0 {
		return "裏で作業中"
	}
	return fmt.Sprintf("裏で作業中 子%d", children)
}

// quietPeerKind reports child notes that never start a parent turn.
func quietPeerKind(kind string) bool { return kind == "ready" || kind == "progress" }
