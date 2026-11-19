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
	Delivered map[operation.ID]bool     `json:",omitempty"`
	Finishes  map[operation.ID]string   `json:",omitempty"`
	Tails     map[operation.ID][]string `json:",omitempty"`
}

func (d *Index) Has(id operation.ID) bool { return d.Delivered[id] }

func (d *Index) mark(ids []operation.ID, id operation.ID) []operation.ID {
	if d.Has(id) {
		return ids
	}
	if d.Delivered == nil {
		d.Delivered = make(map[operation.ID]bool)
	}
	d.Delivered[id] = true
	delete(d.Finishes, id)
	delete(d.Tails, id)
	return append(ids, id)
}

func terminal(status operation.Status) bool {
	return status == operation.StatusCompleted || status == operation.StatusFailed || status == operation.StatusCanceled
}

// The observation methods return handles whose first delivery was found here.
func (d *Index) ObserveItem(item sessionstore.Item) []operation.ID {
	var delivered []operation.ID
	switch value := item.Data.(type) {
	case inbox.Input:
		if value.Kind == inbox.InputPeer {
			if peer, err := value.DecodePeerMessage(); err == nil {
				return d.ObservePeer(peer)
			}
		}
	case sessionstore.ToolCallStatus:
		for _, op := range value.Operations {
			if terminal(op.Status) {
				if _, ok := startState(op); ok {
					delivered = d.mark(delivered, op.ID) // the parent's terminal tool receipt, not SaveOperation
					continue
				}
			}
			delivered = append(delivered, d.ObserveOperation(op)...)
		}
	}
	return delivered
}

// ObservePeer indexes a decoded, validated peer message.
func (d *Index) ObservePeer(peer inbox.PeerMessage) []operation.ID {
	if peer.Kind != "ended" {
		return nil
	}
	delivered := d.mark(nil, operation.ID(peer.Handle))
	if peer.ReportIncluded {
		return delivered
	}
	// Before 16c78d4 one ended input could contain several reports. Its
	// ID, Sender and Handle identified only the first child. Match each
	// additional report's exact header and footer.
	var id operation.ID
	for line := range strings.SplitSeq(peer.Text, "\n") {
		if rest, ok := strings.CutPrefix(line, "Child "); ok {
			if handle, status, ok := strings.Cut(rest, " ended: "); ok && terminal(operation.Status(status)) {
				id = operation.ID(handle)
			}
		}
		if id != "" && line == "(Full report: subagent show "+string(id)+")" {
			delivered = d.mark(delivered, id)
			id = ""
		}
	}
	return delivered
}

func startState(op operation.Operation) (operation.RemoteJobState, bool) {
	if op.Type != operation.TypeRemoteJob {
		return operation.RemoteJobState{}, false
	}
	state, err := operation.DecodeRemoteJobState(op)
	var plan struct{ Action string }
	ok := err == nil && state.Plan.Type == "subagent" && json.Unmarshal(state.Plan.Data, &plan) == nil && plan.Action == "start"
	return state, ok
}

func (d *Index) ObserveOperation(op operation.Operation) []operation.ID {
	if !terminal(op.Status) {
		return nil
	}
	if !d.Has(op.ID) {
		if state, ok := startState(op); ok {
			return d.observeFinish(op.ID, state)
		}
	}
	if op.Status != operation.StatusCompleted {
		return nil
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
	if report == "" {
		return nil
	}
	var delivered []operation.ID
	// CLI show/wait headings carry the end status, including canceled children
	// without Finish. Running reports and listings are not end delivery.
	for line := range strings.SplitSeq(report, "\n") {
		if strings.HasPrefix(line, "== ") {
			words := strings.Fields(line)
			if len(words) >= 4 && terminal(operation.Status(words[len(words)-1])) {
				delivered = d.mark(delivered, operation.ID(words[1]))
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
		if expected, ok := d.Finishes[id]; ok && tail == expected {
			delivered = d.mark(delivered, id)
		} else {
			if d.Tails == nil {
				d.Tails = make(map[operation.ID][]string)
			}
			d.Tails[id] = append(d.Tails[id], tail)
		}
	}
	return delivered
}

func (d *Index) observeFinish(id operation.ID, state operation.RemoteJobState) []operation.ID {
	var handle struct{ Finish *sessionstore.FinishRecord }
	if json.Unmarshal(state.Handle, &handle) != nil || handle.Finish == nil {
		return nil
	}
	tail := finishTail(handle.Finish.Result)
	for _, candidate := range d.Tails[id] {
		if candidate == tail {
			return d.mark(nil, id)
		}
	}
	if d.Finishes == nil {
		d.Finishes = make(map[operation.ID]string)
	}
	d.Finishes[id] = tail
	return nil
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
