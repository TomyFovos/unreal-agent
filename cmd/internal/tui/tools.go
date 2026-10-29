package tui

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	files "github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
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
		case operation.StatusReady:
			if status != operation.StatusCanceled && status != operation.StatusCanceling {
				status = operation.StatusReady
			}
		case operation.StatusCanceling:
			if status != operation.StatusCanceled {
				status = operation.StatusCanceling
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
	if state, err := operation.DecodeRemoteJobState(op); err == nil && state.Plan.Type == files.PlanType && state.Plan.Version == files.Version {
		var result struct {
			Reason mutation.Reason `json:"reason"`
		}
		if json.Unmarshal(state.Handle, &result) == nil && result.Reason.Valid() {
			return "native reason=" + string(result.Reason)
		}
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
		if u.index != nil {
			if op, ok := u.index.operations[id]; ok {
				latest = append(latest, op)
			}
			continue
		}
		for _, op := range s.Operations {
			if op.ID == id {
				latest = append(latest, op)
			}
		}
	}
	if len(latest) > 0 {
		r.Status = receiptStatus(latest)
		for _, op := range latest {
			reason := ""
			if u.index != nil {
				var ok bool
				reason, ok = u.index.reasons[op.ID]
				if !ok {
					reason = operationReason(op)
					u.index.reasons[op.ID] = reason
				}
			} else {
				reason = operationReason(op)
			}
			if reason != "" {
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
		if end.IsZero() && s.Connected && !operationVisualState(r.Status).ended() {
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

// chipSurface adds the same padding and fallback delimiters as roleLine,
// without translating symbols in user/tool text. Nested badges keep their face.
func chipSurface(content line, role colorRole, t Theme) line {
	base := visualStyle(role)
	content = append(line(nil), content...)
	for i := range content {
		if content[i].style.fg == "" {
			content[i].style.fg = base.fg
		}
		if content[i].style.bg == "" {
			content[i].style.bg = base.bg
		}
	}
	frame := t.roleLine("  ", role)
	return joined(frame[:len(frame)/2], content, frame[len(frame)/2:])
}

func toolTarget(name, target string, width int, t Theme) string {
	switch name {
	case "read", "write", "edit", "Read", "Write", "Edit", "ViewImage", "NotebookEdit":
		// Keep a whole basename when it fits; middle may shorten only the
		// directory part. A long basename itself still uses middle.
		if i := strings.LastIndexAny(target, "/\\"); i >= 0 {
			base := target[i+1:]
			if n := textLine(base, normal).width(); n < width {
				return middle(target[:i+1], width-n, t) + base
			}
		}
		return middle(target, width, t)
	default:
		return tail(target, width, t)
	}
}

func receiptChip(r Receipt, d viewer.Duration, s Snapshot, u UIState, width int, rows []viewer.Row, marks map[session.ID]string) (line, string) {
	state := toolVisualState(r, s)
	name, target := displayTool(r.Name, r.Target)
	var badge line
	spawned := false
	if r.Name == "SubagentStart" {
		for _, child := range rows {
			for _, id := range r.Operations {
				if id != "" && id == child.ParentOperationID {
					state = childVisualState(child, s)
					badge = u.Theme.roleLine("["+marks[child.ID]+"]", badgeColor)
					target, name = childTitle(child), ""
					spawned = true
					break
				}
			}
			if spawned {
				break
			}
		}
	}
	mark, word, color := state.presentation(u.Theme)
	role, suffix := chipColor, ""
	if state == stateFailed {
		role = chipBad
	}
	if !spawned {
		switch state {
		case stateWorking:
			if d.Known {
				suffix = " " + coarseDuration(d.Value)
			}
		case stateCompleted:
		default:
			suffix = " " + word
		}
	}
	room := max(0, min(40, width)-u.Theme.roleLine("  ", role).width())
	prefix := joined(textLine(mark+" ", color), badge)
	if len(badge) > 0 {
		prefix = joined(prefix, textLine(" ", normal))
	}
	// Reserve the state/elapsed suffix before shortening a name or target.
	name = tail(name, max(0, room-prefix.width()-textLine(suffix, normal).width()), u.Theme)
	prefix = joined(prefix, textLine(name, normal))
	if target != "" {
		gap := " "
		if spawned {
			gap = ""
		}
		available := max(0, room-prefix.width()-textLine(suffix+gap, normal).width())
		if spawned {
			target = tail(target, available, u.Theme)
		} else {
			target = toolTarget(name, target, available, u.Theme)
		}
		if target != "" {
			prefix = joined(prefix, textLine(gap+target, normal))
		}
	}
	chip := chipSurface(joined(prefix, textLine(suffix, normal)).clip(room), role, u.Theme)
	reason := ""
	if state == stateFailed && r.Reason != "" {
		reason = tail(strings.SplitN(r.Reason, "\n", 2)[0], width, u.Theme)
	}
	return chip, reason
}

// toolChips keeps call order, collapsing only three or more completed calls
// of the same expanded tool in this response. Active/failed calls stay separate.
func toolChips(e Entry, s Snapshot, u UIState, l layout) []bodyLine {
	type call struct {
		receipt Receipt
		elapsed viewer.Duration
	}
	calls := make([]call, len(e.Calls))
	completed := map[string]int{}
	for i, raw := range e.Calls {
		r, d := currentReceipt(raw, s, u)
		calls[i] = call{r, d}
		if toolVisualState(r, s) == stateCompleted && r.Name != "SubagentStart" {
			name, _ := displayTool(r.Name, r.Target)
			completed[name]++
		}
	}
	rows := children(u, s)
	marks := marksFor(u, s)
	var result []bodyLine
	var row line
	flush := func() {
		if len(row) > 0 {
			result = append(result, bodyLine{text: row})
			row = nil
		}
	}
	grouped := map[string]bool{}
	for _, c := range calls {
		r := c.receipt
		name, _ := displayTool(r.Name, r.Target)
		if r.Name != "SubagentStart" && toolVisualState(r, s) == stateCompleted && completed[name] >= 3 {
			if grouped[name] {
				continue
			}
			grouped[name] = true
			r.Name, r.Target = name, fmt.Sprintf("%s%d", u.Theme.symbol("×", "x"), completed[name])
		}
		chip, reason := receiptChip(r, c.elapsed, s, u, l.text, rows, marks)
		if len(row) > 0 && row.width()+1+chip.width() > l.text {
			flush()
		}
		if len(row) > 0 {
			row = joined(row, textLine(" ", normal))
		}
		row = joined(row, chip)
		if reason != "" {
			flush()
			result = append(result, bodyLine{text: textLine(reason, visualStyle(dimColor))})
		}
	}
	flush()
	return result
}
