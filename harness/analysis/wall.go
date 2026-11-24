package analysis

import (
	"encoding/json/v2"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

type TimeKind uint8

const (
	TimeIdle TimeKind = iota
	TimeUnattributed
	TimeModel
	TimeTools
	TimeChildren
	TimeLimit
	TimeApproval
	TimeKinds
)

type TimeSpan struct {
	Start, End time.Time
	Kind       TimeKind
}

type WallSignal struct {
	Start, End, LastRecord time.Time
	Spans                  []TimeSpan
	HasTurns               bool
	openLimit              time.Time
	openApprovals          map[string]time.Time
}

// IncludeLiveWaits extends an unclosed interval only while the live Host
// confirms the wait. A Session may have stopped before writing the end record.
func (w *WallSignal) IncludeLiveWaits(usageLimit bool, approvals []permission.Pending) {
	if usageLimit {
		w.add(TimeLimit, w.openLimit, time.Time{})
	}
	for _, approval := range approvals {
		w.add(TimeApproval, w.openApprovals[approval.ID], time.Time{})
	}
}

func (w *WallSignal) add(kind TimeKind, start, end time.Time) {
	if !start.IsZero() && (end.IsZero() || end.After(start)) {
		w.Spans = append(w.Spans, TimeSpan{Start: start, End: end, Kind: kind})
	}
}

func (a *Accumulator) wallSignal(tools []ToolSample) WallSignal {
	w := WallSignal{Start: a.created, End: a.metrics.ended, LastRecord: a.lastActivity, HasTurns: len(a.context.invocations) > 0,
		openLimit: a.metrics.waitStart, openApprovals: maps.Clone(a.approvalStarts)}
	for _, turn := range a.context.invocations {
		if turn.at.IsZero() {
			w.HasTurns = false
		}
		w.add(TimeUnattributed, turn.at, turn.end)
	}
	for _, req := range a.metrics.requests {
		if req.Duration.Known {
			w.add(TimeModel, req.At.Add(-time.Duration(req.Duration.Value*float64(time.Second))), req.At)
		}
	}
	for _, sample := range tools {
		if !sample.End.IsZero() {
			kind := TimeTools
			if sample.ChildWait {
				kind = TimeChildren
			}
			w.add(kind, sample.Start, sample.End)
		}
	}
	for id, times := range a.metrics.times {
		if !times.end.IsZero() && operationWaitsForChildren(a.operations[id]) {
			w.add(TimeChildren, times.start, times.end)
		}
	}
	for _, call := range a.calls {
		if call.status == nil {
			continue
		}
		for _, id := range call.status.Status.WaitingFor {
			if a.operations[id].ToolName == "SubagentStart" {
				wait := a.metrics.times[id]
				if !wait.end.IsZero() || terminalCall(call, a.operations) == "" {
					w.add(TimeChildren, wait.start, wait.end)
				}
			}
		}
	}
	for _, span := range a.metrics.waitSpans {
		w.add(TimeLimit, span.start, span.end)
	}
	for _, span := range a.approvalSpans {
		w.add(TimeApproval, span.start, span.end)
	}
	return w
}

func toolWaitsForChildren(call llm.ToolCall) bool {
	input := []byte(call.Arguments)
	if call.Name == tool.ClaudeCodeName {
		var args observed.Arguments
		if json.Unmarshal(input, &args) != nil || args.Tool != tool.BashName {
			return false
		}
		input = args.Input
	} else if call.Name != tool.BashName {
		return false
	}
	var args struct {
		Command string
	}
	return json.Unmarshal(input, &args, json.MatchCaseInsensitiveNames(true)) == nil && childWaitCommand(args.Command)
}

func operationWaitsForChildren(op operation.Operation) bool {
	if op.Type != operation.TypeShell {
		return false
	}
	state, err := operation.DecodeShellState(op)
	return err == nil && childWaitCommand(state.Input.Command)
}

// Inspect command words only. Quoted command examples are arguments, and
// setup commands such as cd or a binary-path assignment can precede the wait.
var shellWords = regexp.MustCompile("[;|&\\n]|(?:'[^']*'|\"(?:\\\\.|[^\"\\\\])*\"|\\\\[^\\n]|[^[:space:]'\";|&\\\\])+")

func childWaitCommand(command string) bool {
	if !strings.Contains(command, "subagent") || !strings.Contains(command, "wait") {
		return false
	}
	isUnreal := func(s string) bool {
		name := filepath.Base(strings.Trim(s, "'\""))
		return name == "unreal-agent" || name == "unreal"
	}
	aliases := make(map[string]bool)
	words := shellWords.FindAllString(command, -1)
	for len(words) > 0 {
		end := slices.IndexFunc(words, func(word string) bool {
			return len(word) == 1 && strings.ContainsRune(";|&\n", rune(word[0]))
		})
		if end < 0 {
			end = len(words)
		}
		part := words[:end]
		words = words[min(end+1, len(words)):]
		for len(part) > 0 {
			name, value, assignment := strings.Cut(part[0], "=")
			if !assignment || name == "" || strings.ContainsAny(name, " /$'\"") {
				break
			}
			aliases[name] = isUnreal(value)
			part = part[1:]
		}
		if len(part) >= 3 && strings.Trim(part[1], "'\"") == "subagent" && strings.Trim(part[2], "'\"") == "wait" {
			binary := strings.Trim(part[0], "'\"")
			if isUnreal(binary) || strings.HasPrefix(binary, "$") && aliases[strings.Trim(binary[1:], "{}")] {
				return true
			}
		}
	}
	return false
}

func (a *Accumulator) observeApproval(r sessionstore.HostRecord, at time.Time) {
	if r.Approval == nil {
		return
	}
	id := r.Approval.Request.ID
	if r.Approval.Decision == "" {
		if a.approvalStarts == nil {
			a.approvalStarts = make(map[string]time.Time)
		}
		a.approvalStarts[id] = at
		return
	}
	if start, ok := a.approvalStarts[id]; ok {
		a.approvalSpans = append(a.approvalSpans, workSpan{start: start, end: at})
		delete(a.approvalStarts, id)
	}
}

// Partition returns non-overlapping wall-clock segments. When phases overlap,
// anchored waits take precedence, followed by tools and generation. The
// residual inside a turn or child lifetime is unattributed.
func (w WallSignal) Partition(end time.Time, complete bool) []TimeSpan {
	if w.Start.IsZero() || !end.After(w.Start) {
		return nil
	}
	type edge struct {
		at    time.Time
		kind  TimeKind
		delta int
	}
	edges := make([]edge, 0, 2*len(w.Spans)+1)
	for _, span := range w.Spans {
		a, b := span.Start, span.End
		if a.Before(w.Start) {
			a = w.Start
		}
		if b.IsZero() || b.After(end) {
			b = end
		}
		if b.After(a) {
			edges = append(edges, edge{a, span.Kind, 1}, edge{b, span.Kind, -1})
		}
	}
	edges = append(edges, edge{at: end})
	slices.SortFunc(edges, func(a, b edge) int { return a.at.Compare(b.at) })
	base := TimeUnattributed
	if complete && w.HasTurns {
		base = TimeIdle
	}
	var active [TimeKinds]int
	var out []TimeSpan
	last := w.Start
	for _, e := range edges {
		if e.at.After(last) {
			kind := base
			for k := TimeApproval; k > TimeIdle; k-- {
				if active[k] > 0 {
					kind = k
					break
				}
			}
			if len(out) > 0 && out[len(out)-1].Kind == kind {
				out[len(out)-1].End = e.at
			} else {
				out = append(out, TimeSpan{Start: last, End: e.at, Kind: kind})
			}
		}
		active[e.kind] += e.delta
		last = e.at
	}
	return out
}

func SpanTotals(spans []TimeSpan, start, end time.Time) (totals [TimeKinds]time.Duration) {
	for _, span := range spans {
		a, b := span.Start, span.End
		if a.Before(start) {
			a = start
		}
		if b.After(end) {
			b = end
		}
		if b.After(a) {
			totals[span.Kind] += b.Sub(a)
		}
	}
	return totals
}
