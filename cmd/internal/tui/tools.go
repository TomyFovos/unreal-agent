package tui

import (
	"encoding/json/v2"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// A receipt keeps call linkage and whitelisted display arguments, never a raw
// argument/state dump. Operation IDs are internal linkage, never main UI text.
type Receipt struct {
	CallID, Name, Target, Reason string
	Operations                   []operation.ID
	Status                       operation.Status
	StartedAt, EndedAt           time.Time
}

func newReceipt(c llm.ToolCall) Receipt {
	r := Receipt{CallID: c.CallID, Name: clipBytes(c.Name, 128), Status: operation.StatusAwaiting}
	r.Target = viewer.SafeToolTarget(c)
	return r
}
func terminalStatus(s operation.Status) bool {
	return s == operation.StatusCompleted || s == operation.StatusFailed || s == operation.StatusCanceled
}
func receiptStatus(ops []operation.Operation) operation.Status {
	status := operation.StatusCompleted
	for _, op := range ops {
		switch op.Status {
		case operation.StatusFailed:
			return operation.StatusFailed
		case operation.StatusCanceled:
			status = operation.StatusCanceled
		case operation.StatusReady, operation.StatusCanceling:
			if status != operation.StatusCanceled {
				status = operation.StatusReady
			}
		case operation.StatusAwaiting:
			if status == operation.StatusCompleted {
				status = operation.StatusAwaiting
			}
		}
	}
	return status
}
func operationReason(op operation.Operation) string {
	if op.Denial != nil {
		return clipBytes(op.Denial.Error(), 4096)
	}
	// Known terminal-error field only. Tool stdout, stderr and opaque state stay out.
	var state struct{ TerminalError string }
	if json.Unmarshal(op.State, &state) == nil {
		return clipBytes(state.TerminalError, 4096)
	}
	return ""
}
func updateReceipts(entries []Entry, d sessionstore.ToolCallStatus, at time.Time) {
	for i := range entries {
		for j := range entries[i].Calls {
			r := &entries[i].Calls[j]
			if r.CallID != d.CallID {
				continue
			}
			r.Operations = append([]operation.ID(nil), d.Status.WaitingFor...)
			for _, op := range d.Operations {
				found := false
				for _, id := range r.Operations {
					found = found || id == op.ID
				}
				if !found {
					r.Operations = append(r.Operations, op.ID)
				}
			}
			r.Status = receiptStatus(d.Operations)
			if len(d.Operations) == 0 && len(d.Status.WaitingFor) > 0 {
				r.Status = operation.StatusAwaiting
			}
			if d.Status.Error != "" || d.Status.Denial != nil {
				r.Status = operation.StatusFailed
				r.Reason = clipBytes(d.Status.Error, 4096)
				if d.Status.Denial != nil {
					r.Reason = clipBytes(d.Status.Denial.Error(), 4096)
				}
			}
			for _, op := range d.Operations {
				if reason := operationReason(op); reason != "" {
					r.Reason = reason
				}
			}
			if !at.IsZero() {
				if r.Status == operation.StatusReady && r.StartedAt.IsZero() {
					r.StartedAt = at
				}
				if terminalStatus(r.Status) {
					r.EndedAt = at
				}
			}
		}
	}
}
func currentReceipt(r Receipt, s Snapshot, u UIState) (Receipt, viewer.Duration) {
	var latest []operation.Operation
	for _, id := range r.Operations {
		for _, op := range s.Operations {
			if op.ID == id {
				latest = append(latest, op)
			}
		}
	}
	if len(latest) > 0 {
		r.Status = receiptStatus(latest)
		for _, op := range latest {
			if reason := operationReason(op); reason != "" {
				r.Reason = reason
			}
		}
	}
	d := viewer.Duration{}
	if u.Parent != nil {
		for _, op := range u.Parent.Operations {
			for _, id := range r.Operations {
				if op.ID == id && op.Elapsed.Known && (!d.Known || op.Elapsed.Value > d.Value) {
					d = op.Elapsed
				}
			}
		}
	}
	if !d.Known && !r.StartedAt.IsZero() {
		end := r.EndedAt
		if end.IsZero() && s.Connected && r.Status == operation.StatusReady {
			end = u.Now
		}
		if !end.IsZero() && !end.Before(r.StartedAt) {
			d = viewer.Duration{Known: true, Value: end.Sub(r.StartedAt)}
		}
	}
	if !s.Connected {
		r.Status = "unknown"
		d = viewer.Duration{}
	}
	return r, d
}
func stateMark(s operation.Status, t Theme) (string, string, style) {
	switch s {
	case operation.StatusAwaiting:
		return t.symbol("◌", "."), "queued", meta
	case operation.StatusReady, operation.StatusCanceling:
		return t.symbol("▸", ">"), "running", liveStyle
	case operation.StatusCompleted:
		return t.symbol("✓", "+"), "", meta
	case operation.StatusFailed:
		return t.symbol("✗", "x"), "failed", failureStyle
	case operation.StatusCanceled:
		return t.symbol("⊘", "-"), "canceled", normal
	default:
		return "?", "unknown", warningStyle
	}
}
func toolRows(e Entry, s Snapshot, u UIState, l layout) []bodyLine {
	var result []bodyLine
	completed := map[string]int{}
	n := 0
	for _, raw := range e.Calls {
		r, _ := currentReceipt(raw, s, u)
		if r.Status == operation.StatusCompleted && r.Name != "SubagentStart" {
			completed[r.Name]++
			n++
		}
	}
	if n >= 2 {
		var names []string
		for name := range completed {
			names = append(names, name)
		}
		sort.Strings(names)
		var labels []string
		for _, name := range names {
			labels = append(labels, fmt.Sprintf("%s %d", name, completed[name]))
		}
		text := fmt.Sprintf("%s %d tool calls: %s", u.Theme.symbol("✓", "+"), n, strings.Join(labels, ", "))
		result = append(result, bodyLine{text: textLine(middle(text, l.text, u.Theme), meta)})
	}
	for _, raw := range e.Calls {
		r, d := currentReceipt(raw, s, u)
		if n >= 2 && r.Status == operation.StatusCompleted && r.Name != "SubagentStart" {
			continue
		}
		name, target := r.Name, r.Target
		var spawned *viewer.Row
		if name == "SubagentStart" {
			name = "spawn"
			for _, child := range children(u, s) {
				for _, id := range r.Operations {
					if id == child.ParentOperationID {
						target = string(child.ID) + "  " + child.Label
						copy := child
						spawned = &copy
					}
				}
			}
		}
		mark, word, st := stateMark(r.Status, u.Theme)
		if spawned != nil {
			mark, word, st, _ = childState(*spawned, s, u.Theme)
		}
		if word == "running" {
			word = ""
		}
		if d.Known && (r.Status == operation.StatusReady && (spawned == nil || spawned.Finish == nil) || r.Status == operation.StatusCompleted && d.Value >= 5*time.Second) {
			if word != "" {
				word += "  "
			}
			word += duration(d.Value)
		}
		suffix := ""
		if word != "" {
			suffix = "  " + word
		}
		prefix := mark + " " + name
		room := max(0, l.text-len(suffix)-textLine(prefix, normal).width()-2)
		if target != "" {
			prefix += "  " + middle(target, room, u.Theme)
		}
		bodyStyle := normal
		if r.Status == operation.StatusCompleted && spawned == nil {
			bodyStyle = meta
		}
		markStyle := st
		if r.Status == operation.StatusCompleted && spawned == nil {
			markStyle = successStyle
			markStyle.faint = true
		}
		row := joined(textLine(mark, markStyle), textLine(strings.TrimPrefix(prefix, mark), bodyStyle))
		suffixStyle := meta
		if word == "failed" || word == "unknown" {
			suffixStyle = st
		}
		row = joined(row, textLine(strings.Repeat(" ", max(0, l.text-row.width()-textLine(suffix, meta).width()))+suffix, suffixStyle))
		result = append(result, bodyLine{text: row.clip(l.text)})
		if r.Reason != "" && (r.Status == operation.StatusFailed || r.Status == operation.StatusCanceled) {
			result = append(result, bodyLine{text: textLine(middle(r.Reason, l.text, u.Theme), meta)})
		}
	}
	return result
}
