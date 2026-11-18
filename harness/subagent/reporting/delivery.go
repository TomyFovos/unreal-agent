// Package reporting projects child-report delivery from canonical parent records.
package reporting

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

// Index is a replayable projection, not a delivery checkpoint. A terminal start
// checkpoint or a child's Finish alone does not establish parent delivery.
// Callers serialize access with their existing Session/event-loop ownership.
type Index struct {
	delivered map[operation.ID]bool
	finishes  map[operation.ID]string
	tails     map[operation.ID][]string
}

func (d *Index) Has(id operation.ID) bool { return d.delivered[id] }

func (d *Index) mark(id operation.ID) {
	if d.delivered == nil {
		d.delivered = make(map[operation.ID]bool)
	}
	d.delivered[id] = true
	delete(d.finishes, id)
	delete(d.tails, id)
}

func terminal(status operation.Status) bool {
	return status == operation.StatusCompleted || status == operation.StatusFailed || status == operation.StatusCanceled
}

func (d *Index) ObserveItem(item sessionstore.Item) {
	switch value := item.Data.(type) {
	case inbox.Input:
		peer, err := value.DecodePeerMessage()
		if err != nil || peer.Kind != "ended" {
			return
		}
		d.mark(operation.ID(peer.Handle))
		// Before 16c78d4 one ended input could contain several reports. Its
		// ID, Sender and Handle identified only the first child. Match each
		// additional report's exact header AND footer, not handle mentions.
		var id operation.ID
		for line := range strings.SplitSeq(peer.Text, "\n") {
			if rest, ok := strings.CutPrefix(line, "Child "); ok {
				if handle, status, ok := strings.Cut(rest, " ended: "); ok && terminal(operation.Status(status)) {
					id = operation.ID(handle)
				}
			}
			if id != "" && line == "(Full report: subagent show "+string(id)+")" {
				d.mark(id)
				id = ""
			}
		}
	case sessionstore.ToolCallStatus:
		for _, op := range value.Operations {
			if _, ok := startState(op); ok && terminal(op.Status) {
				d.mark(op.ID) // the parent's terminal tool receipt, not SaveOperation
			}
			d.ObserveOperation(op)
		}
	}
}

func startState(op operation.Operation) (operation.RemoteJobState, bool) {
	state, err := operation.DecodeRemoteJobState(op)
	var plan struct{ Action string }
	ok := err == nil && state.Plan.Type == "subagent" && json.Unmarshal(state.Plan.Data, &plan) == nil && plan.Action == "start"
	return state, ok
}

func (d *Index) ObserveOperation(op operation.Operation) {
	if state, ok := startState(op); ok && terminal(op.Status) && !d.Has(op.ID) {
		var handle struct{ Finish *sessionstore.FinishRecord }
		if json.Unmarshal(state.Handle, &handle) == nil && handle.Finish != nil {
			tail := finishTail(handle.Finish.Result)
			for _, candidate := range d.tails[op.ID] {
				if candidate == tail {
					d.mark(op.ID)
					return
				}
			}
			if d.finishes == nil {
				d.finishes = make(map[operation.ID]string)
			}
			d.finishes[op.ID] = tail
		}
	}
	if op.Status != operation.StatusCompleted {
		return
	}
	var report string
	switch op.Type {
	case operation.TypeValue:
		value, err := operation.DecodeValue(op)
		var result observed.Result
		if err == nil && json.Unmarshal(value, &result) == nil && !result.IsError {
			report = result.Content
		}
	case operation.TypeShell:
		state, err := operation.DecodeShellState(op)
		if err == nil && state.Result != nil && state.Result.ExitCode == 0 {
			report = state.Result.Out
		}
	}
	// CLI show/wait headings carry the end status, including canceled children
	// without Finish. Running reports and listings are not end delivery.
	for line := range strings.SplitSeq(report, "\n") {
		if strings.HasPrefix(line, "== ") {
			words := strings.Fields(line)
			if len(words) >= 4 && terminal(operation.Status(words[len(words)-1])) {
				d.mark(operation.ID(words[1]))
			}
		}
	}
	// Historical Bash receipts also contain `echo "=== $h"; show $h | tail`.
	// The result/status heading can be absent. Require the exact full trailing
	// fields of that child's canonical Finish; a handle or prose alone cannot
	// suppress delivery. Join both record orders during replay.
	for _, section := range strings.Split("\n"+report, "\n=== ")[1:] {
		header, body, ok := strings.Cut(section, "\n")
		if !ok || len(strings.Fields(header)) != 1 {
			continue
		}
		_, tail, ok := strings.Cut("\n"+body, "\nchanged files:")
		if !ok {
			continue
		}
		id := operation.ID(header)
		if d.Has(id) {
			continue
		}
		tail = strings.TrimSpace("changed files:" + tail)
		if expected, ok := d.finishes[id]; ok && tail == expected {
			d.mark(id)
		} else {
			if d.tails == nil {
				d.tails = make(map[operation.ID][]string)
			}
			d.tails[id] = append(d.tails[id], tail)
		}
	}
}

// This is the existing subagent show/wait footer format, including empty lists.
func finishTail(result sessionstore.FinishResult) string {
	var text strings.Builder
	for _, list := range []struct {
		name   string
		values []string
	}{{"changed files", result.ChangedFiles}, {"tests", result.Tests}, {"blockers", result.Blockers}} {
		if len(list.values) == 0 {
			fmt.Fprintf(&text, "%s: none\n", list.name)
		} else {
			fmt.Fprintf(&text, "%s:\n", list.name)
			for _, value := range list.values {
				fmt.Fprintf(&text, "  - %s\n", value)
			}
		}
	}
	return strings.TrimSpace(text.String())
}
