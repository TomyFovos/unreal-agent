package tui

import (
	"fmt"
	"strings"
	"time"
)

func pulse(s Snapshot, u UIState, l layout) line {
	if !s.Connected || !s.Running || runtime(s) == "failed" {
		return nil
	}
	phase := ""
	elapsed := time.Duration(0)
	known := false
	if p := s.Progress; p != nil {
		switch {
		case p.Attempt >= 2 && p.Text == "":
			phase = fmt.Sprintf("retrying (attempt %d)", p.Attempt)
		case p.Mode == "streaming" && p.Text != "":
			phase = "generating"
			if p.Attempt >= 2 {
				phase += fmt.Sprintf(" (attempt %d)", p.Attempt)
			}
		case p.Mode == "streaming":
			phase = "thinking"
		default:
			phase = "waiting for completed response"
		}
	} else {
		var tools []string
		for _, op := range s.Operations {
			if !terminalStatus(op.Status) && op.ToolName != "SubagentStart" {
				tools = append(tools, op.ToolName)
				if u.Parent != nil {
					for _, r := range u.Parent.Operations {
						if r.ID == op.ID && r.Elapsed.Known && (!known || r.Elapsed.Value > elapsed) {
							known = true
							elapsed = r.Elapsed.Value
						}
					}
				}
			}
		}
		if len(tools) == 1 {
			phase = "running " + tools[0]
		} else if len(tools) > 1 {
			phase = fmt.Sprintf("running %d tools", len(tools))
		} else if s.WaitingForModel {
			phase = "thinking"
		}
	}
	if phase == "" {
		return nil
	}
	if !strings.HasPrefix(phase, "running ") && !s.LatestAt.IsZero() && !u.Now.Before(s.LatestAt) {
		known = true
		elapsed = u.Now.Sub(s.LatestAt)
	}
	text := textLine(phase, normal)
	if known {
		text = joined(text, textLine("  "+duration(elapsed), meta))
	}
	return l.gutter("", u.Theme.spinner(u.Now.UnixMilli()), text)
}

func stateDock(s Snapshot, u UIState, l layout) []line {
	var result []line
	conn := connection(s, u.Now)
	if !s.Connected {
		if conn == "resyncing" {
			return []line{l.gutter("", "", textLine("rebuilding the view from canonical history", meta))}
		}
		if conn == "disconnected" {
			elapsed := ""
			if !s.OfflineSince.IsZero() {
				elapsed = "  " + duration(u.Now.Sub(s.OfflineSince))
			}
			text := "reconnecting" + elapsed + "  session state is unknown until the host answers"
			return []line{l.gutter("", u.Theme.spinner(u.Now.UnixMilli()), textLine(middle(text, l.text, u.Theme), warningStyle))}
		}
		return nil
	}
	switch runtime(s) {
	case "stopped":
		result = append(result, l.gutter("", u.Theme.symbol("▪", "#"), textLine("session stopped; /resume continues this session", strong)))
	case "failed":
		failure := s.Failure
		if failure == "" {
			failure = strings.TrimPrefix(s.Status, "session failed: ")
		}
		for i, v := range wrapLine(textLine("session failed: "+failure, failureStyle), l.text, false, 0) {
			if i >= 3 {
				break
			}
			mark := ""
			if i == 0 {
				mark = u.Theme.symbol("✗", "x")
			}
			result = append(result, l.gutter("", mark, v))
		}
	}
	if s.PendingSelection != nil {
		text := "next turn: " + topologyText(selectionLabel(*s.PendingSelection, u.Theme), 256)
		result = append(result, l.gutter("", "", textLine(middle(text, l.text, u.Theme), meta)))
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
