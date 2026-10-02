package viewer

import (
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type responseKey struct {
	Turn     session.TurnID
	ID       string
	Sequence sessionstore.Sequence
}

type timing struct{ start, end time.Time }
type projection struct {
	view       host.View
	cursor     sessionstore.Sequence
	recent     []host.HistoryItem
	operations map[operation.ID]operation.Operation
	times      map[operation.ID]timing
	responses  map[responseKey]bool
	usage      Usage
	finish     *Finish
	activity   string
	runtime    Liveness
	resync     bool
	more       bool
	problem    string
}
type Model struct {
	mu       sync.Mutex
	options  Options
	sessions map[session.ID]*projection
	selected session.ID
}

func New(options Options) *Model {
	if options.RecentLimit <= 0 {
		options.RecentLimit = 128
	}
	if options.RecentLimit > 4096 {
		options.RecentLimit = 4096
	}
	return &Model{options: options, sessions: map[session.ID]*projection{}}
}
func copyValue[T any](v T) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
func validPage(after sessionstore.Sequence, page host.HistoryPage) bool {
	cursor := after
	for _, item := range page.Items {
		if cursor == sessionstore.Sequence(math.MaxUint64) || item.Sequence != cursor+1 {
			return false
		}
		cursor = item.Sequence
	}
	return page.NextAfter == cursor && (!page.More || len(page.Items) > 0)
}

// Replace starts a subscription epoch from sequence 1. Only the current Watch
// may call it; delayed responses from old subscriptions must not start epochs.
func (m *Model) Replace(id session.ID, view host.View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" || view.Session.Session.ID != id || !validPage(0, view.History) {
		return ErrInvalidPage
	}
	v, err := copyValue(view)
	if err != nil {
		return err
	}
	p := &projection{operations: map[operation.ID]operation.Operation{}, times: map[operation.ID]timing{}, responses: map[responseKey]bool{}, runtime: RuntimeUnknown}
	m.sessions[id] = p
	if m.selected == "" {
		m.selected = id
	}
	m.snapshot(p, v)
	for _, item := range v.History.Items {
		if err = m.item(p, item); err != nil {
			p.resync = true
			p.runtime = RuntimeUnknown
			return err
		}
	}
	p.more = v.History.More
	return nil
}
func (m *Model) snapshot(p *projection, v host.View) {
	p.view.Session = v.Session
	p.view.Generation = v.Generation
	p.view.Revision = v.Revision
	p.view.Running = v.Running
	p.view.Failure = v.Failure
	p.view.ProjectInstructions = v.ProjectInstructions
	p.operations = map[operation.ID]operation.Operation{}
	for _, op := range v.Operations {
		p.operations[op.ID] = op
	}
	p.runtime = RuntimeUnknown
	if v.Generation != "" {
		p.runtime = RuntimeStopped
		if v.Running {
			p.runtime = RuntimeRunning
		}
	}
}
func (m *Model) AppendPage(id session.ID, after sessionstore.Sequence, view host.View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.sessions[id]
	if p == nil || p.resync {
		return ErrResync
	}
	if view.Session.Session.ID != id || view.Generation != p.view.Generation {
		p.resync = true
		p.runtime = RuntimeUnknown
		return ErrStale
	}
	if after != p.cursor || !validPage(after, view.History) {
		p.resync = true
		p.runtime = RuntimeUnknown
		return ErrInvalidPage
	}
	if view.Revision < p.view.Revision {
		return ErrStale
	}
	v, err := copyValue(view)
	if err != nil {
		return err
	}
	m.snapshot(p, v)
	for _, item := range v.History.Items {
		if err = m.item(p, item); err != nil {
			p.resync = true
			p.runtime = RuntimeUnknown
			return err
		}
	}
	p.more = v.History.More
	return nil
}

// Apply accepts only the next post-commit event. Old revisions/generations cannot
// overwrite state. A missing revision or history sequence requires resync.
func (m *Model) Apply(id session.ID, event host.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.sessions[id]
	if p == nil || p.resync {
		return ErrResync
	}
	if event.Generation != p.view.Generation || event.Revision <= p.view.Revision {
		return ErrStale
	}
	if event.Kind == "gap" || event.Revision != p.view.Revision+1 || p.more {
		p.resync = true
		p.runtime = RuntimeUnknown
		return ErrResync
	}
	e, err := copyValue(event)
	if err != nil {
		return err
	}
	switch e.Kind {
	case "item":
		if e.Item == nil || e.Item.Sequence != p.cursor+1 {
			p.resync = true
			p.runtime = RuntimeUnknown
			return ErrResync
		}
		if err = m.item(p, *e.Item); err != nil {
			p.resync = true
			p.runtime = RuntimeUnknown
			return err
		}
		if status, ok := e.Item.Data.(sessionstore.ToolCallStatus); ok {
			for _, op := range status.Operations {
				p.operations[op.ID] = op
			}
		}
	case "operation":
		if e.Operation == nil || e.Operation.ID == "" {
			p.resync = true
			p.runtime = RuntimeUnknown
			return ErrResync
		}
		p.operations[e.Operation.ID] = *e.Operation
	case "progress":
		// Streaming text is transient. Advancing its revision keeps the stream
		// contiguous without inventing canonical activity, usage, or Finish.
	case "stopped":
		p.runtime = RuntimeStopped
		p.view.Running = false
	default:
		p.resync = true
		p.runtime = RuntimeUnknown
		return ErrResync
	}
	p.view.Revision = e.Revision
	return nil
}
func (m *Model) Disconnect(id session.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.sessions[id]; p != nil {
		p.runtime = RuntimeUnknown
	}
}
func (m *Model) Invalidate(id session.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.sessions[id]; p != nil {
		p.runtime = RuntimeUnknown
		p.resync = true
	}
}
func (m *Model) item(p *projection, item host.HistoryItem) error {
	if item.Sequence != p.cursor+1 {
		return ErrInvalidPage
	}
	if m.options.DecodeFinish != nil {
		f, err := m.options.DecodeFinish(item)
		if err != nil {
			return err
		}
		if f != nil {
			if p.finish != nil {
				return fmt.Errorf("multiple canonical Finish records")
			}
			p.finish = f
			p.finish.RecordedAt = item.RecordedAt
		}
	}
	p.cursor = item.Sequence
	p.recent = append(p.recent, item)
	if len(p.recent) > m.options.RecentLimit {
		p.recent = append([]host.HistoryItem(nil), p.recent[len(p.recent)-m.options.RecentLimit:]...)
	}
	switch data := item.Data.(type) {
	case sessionstore.ModelResponse:
		key := responseKey{Turn: data.TurnID, ID: data.Response.ID}
		if data.Response.ID == "" {
			key = responseKey{Sequence: item.Sequence}
		}
		if !p.responses[key] {
			p.responses[key] = true
			addUsage(&p.usage, data.Response.Usage)
		}
		for _, out := range data.Response.Output {
			if msg, ok := out.Data.(llm.Message); ok && msg.Role == llm.RoleAssistant {
				p.activity = short(msg.Text)
			}
			if call, ok := out.Data.(llm.ToolCall); ok {
				p.activity = "tool: " + short(call.Name)
			}
		}
	case inbox.Input:
		switch data.Kind {
		case inbox.InputExternal:
			var text string
			if json.Unmarshal(data.Payload, &text) == nil {
				p.activity = "input: " + short(text)
			}
		case inbox.InputControl:
			p.activity = "control received"
		case inbox.InputCrash:
			p.activity = "crash recorded"
		}
	case sessionstore.ToolCallStatus:
		for _, op := range data.Operations {
			t := p.times[op.ID]
			if !item.RecordedAt.IsZero() {
				if op.Status == operation.StatusReady && t.start.IsZero() {
					t.start = item.RecordedAt
				}
				if terminal(op.Status) {
					t.end = item.RecordedAt
				}
			}
			p.times[op.ID] = t
		}
	}
	return nil
}
func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "..."
	}
	return s
}
func addUsage(total *Usage, u llm.Usage) {
	total.Responses++
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteInputTokens < 0 || u.ReasoningTokens < 0 {
		total.Partial = true
		return
	}
	known := len(u.Raw) > 0 && string(u.Raw) != "null" || u.InputTokens != 0 || u.OutputTokens != 0 || u.CachedInputTokens != 0 || u.CacheWriteInputTokens != 0 || u.ReasoningTokens != 0
	if !known {
		total.Partial = true
		return
	}
	values := []int64{u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens, u.OutputTokens, u.ReasoningTokens}
	targets := []*int64{&total.Input, &total.CachedInput, &total.CacheWriteInput, &total.Output, &total.Reasoning}
	for i, v := range values {
		if *targets[i] > math.MaxInt64-v {
			total.Partial = true
			return
		}
	}
	total.Known = true
	for i, v := range values {
		*targets[i] += v
	}
}
func terminal(s operation.Status) bool {
	return s == operation.StatusCompleted || s == operation.StatusFailed || s == operation.StatusCanceled
}
func elapsed(start, end, now time.Time, runtime Liveness) Duration {
	if start.IsZero() {
		return Duration{}
	}
	if end.IsZero() {
		if runtime != RuntimeRunning {
			return Duration{}
		}
		end = now
	}
	if end.Before(start) {
		return Duration{}
	}
	return Duration{Known: true, Value: end.Sub(start)}
}
func (m *Model) base(id session.ID, p *projection, now time.Time) Row {
	r := Row{ID: id, Runtime: RuntimeUnknown, More: true}
	if p == nil {
		return r
	}
	r.ProjectInstructions = p.view.ProjectInstructions
	r.Generation = p.view.Generation
	r.Runtime = p.runtime
	r.Failure = p.view.Failure
	r.Activity = p.activity
	r.Usage = p.usage
	r.Usage.Partial = r.Usage.Partial || p.more || p.resync
	r.Cursor = p.cursor
	r.More = p.more
	r.NeedsResync = p.resync
	r.Problem = p.problem
	r.Finish = p.finish
	end := time.Time{}
	if p.finish != nil {
		end = p.finish.RecordedAt
	}
	r.Elapsed = elapsed(p.view.Session.Session.CreatedAt, end, now, p.runtime)
	for _, op := range p.operations {
		t := p.times[op.ID]
		runtime := p.runtime
		if terminal(op.Status) && t.end.IsZero() {
			runtime = RuntimeUnknown
		}
		r.Operations = append(r.Operations, OperationRow{ID: op.ID, Tool: op.ToolName, Type: op.Type, Status: op.Status, Elapsed: elapsed(t.start, t.end, now, runtime)})
	}
	sort.Slice(r.Operations, func(i, j int) bool { return r.Operations[i].ID < r.Operations[j].ID })
	return r
}
func (m *Model) rows(now time.Time) []Row {
	byID := map[session.ID]Row{}
	for id, p := range m.sessions {
		byID[id] = m.base(id, p, now)
	}
	ids := make([]session.ID, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if m.options.DecodeChild != nil {
		for _, parent := range ids {
			p := m.sessions[parent]
			ops := make([]operation.Operation, 0, len(p.operations))
			for _, op := range p.operations {
				ops = append(ops, op)
			}
			sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
			for _, op := range ops {
				child, ok, err := m.options.DecodeChild(parent, op)
				if err != nil {
					r := byID[parent]
					r.Problem = "invalid child plan: " + err.Error()
					byID[parent] = r
					continue
				}
				if !ok {
					continue
				}
				if child.ID == "" || child.ID == parent {
					r := byID[parent]
					r.Problem = "invalid child identity"
					byID[parent] = r
					continue
				}
				r, exists := byID[child.ID]
				if !exists {
					r = m.base(child.ID, nil, now)
				}
				if r.ParentID != "" && (r.ParentID != parent || r.ParentOperationID != op.ID) {
					r.Problem = "child has conflicting parent operations"
					byID[child.ID] = r
					continue
				}
				r.ParentID = parent
				r.ParentOperationID = op.ID
				r.ParentOperationStatus = op.Status
				r.Label = child.Label

				byID[child.ID] = r
			}
		}
	}
	children := map[session.ID][]session.ID{}
	for id, r := range byID {
		children[r.ParentID] = append(children[r.ParentID], id)
	}
	for p := range children {
		sort.Slice(children[p], func(i, j int) bool { return children[p][i] < children[p][j] })
	}
	var rows []Row
	visited := map[session.ID]bool{}
	var walk func(session.ID, int)
	walk = func(id session.ID, depth int) {
		if visited[id] {
			return
		}
		visited[id] = true
		r := byID[id]
		r.Depth = depth
		rows = append(rows, r)
		for _, child := range children[id] {
			walk(child, depth+1)
		}
	}
	for _, id := range children[""] {
		walk(id, 0)
	}
	all := make([]session.ID, 0, len(byID))
	for id := range byID {
		all = append(all, id)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	for _, id := range all {
		if !visited[id] {
			r := byID[id]
			r.Problem = "cyclic parent lineage"
			byID[id] = r
		}
	}
	for _, id := range all {
		if !visited[id] {
			walk(id, 0)
		}
	}

	return rows
}
func (m *Model) Rows(now time.Time) []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.rows(now)
	for i := range rows {
		rows[i] = cloneRow(rows[i])
	}
	return rows
}
func (m *Model) Select(id session.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows(time.Now()) {
		if r.ID == id {
			m.selected = id
			return nil
		}
	}
	return fmt.Errorf("unknown session %q", id)
}
func (m *Model) Selected() session.ID { m.mu.Lock(); defer m.mu.Unlock(); return m.selected }
func (m *Model) Detail(id session.ID, now time.Time) (Detail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows(now) {
		if r.ID == id {
			d := Detail{Row: cloneRow(r)}
			if p := m.sessions[id]; p != nil {
				var err error
				d.Recent, err = copyValue(p.recent)
				if err != nil {
					return Detail{}, false
				}
				if len(p.recent) > 0 {
					d.RecentAfter = p.recent[0].Sequence - 1
				}
			}
			return d, true
		}
	}
	return Detail{}, false
}

func cloneRow(r Row) Row {
	if r.ProjectInstructions != nil {
		metadata := *r.ProjectInstructions
		r.ProjectInstructions = &metadata
	}
	r.Operations = append([]OperationRow(nil), r.Operations...)
	if r.Finish != nil {
		f := *r.Finish
		f.ChangedFiles = append([]string(nil), f.ChangedFiles...)
		f.Tests = append([]string(nil), f.Tests...)
		f.Blockers = append([]string(nil), f.Blockers...)
		r.Finish = &f
	}
	return r
}
