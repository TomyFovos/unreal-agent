// Package tui is a disposable projection of Host history and snapshots. It never
// opens a Session store and never performs model/tool work.
package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type Reader interface {
	Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)
	Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error)
}
type Snapshot struct {
	ID                 session.ID
	Generation         string
	Revision           uint64
	After              sessionstore.Sequence
	Running, Connected bool
	Status             string
	Failure            string
	OfflineSince       time.Time
	Entries            []Entry
	OlderDropped       bool
	LatestKind         sessionstore.ItemKind
	LatestAt           time.Time
	Lines              []string
	Operations         []operation.Operation
	Progress           *host.Progress
	WaitingForModel    bool
	Selection          *sessionstore.RuntimeSelection
	PendingSelection   *sessionstore.RuntimeSelection
	ContextPackage     *contextengine.Diagnostics
	TurnID             session.TurnID // observed canonical turn, for frontend topology
}
type Model struct {
	mu                sync.Mutex
	state             Snapshot
	completedEpoch    uint64
	hasCompletedEpoch bool
	seenEpoch         uint64
	analysis          *analysis.Accumulator
	historyMore       bool
	capabilities      map[string]modelcatalog.Capabilities
}

func NewModel(id session.ID) *Model {
	return &Model{state: Snapshot{ID: id, Status: "connecting"}, analysis: analysis.New(id)}
}
func (m *Model) Analysis(now time.Time, rows []viewer.Row) analysis.Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.analysis.Snapshot(now, m.state.Connected, strings.Contains(m.state.Status, "resync"), rows, m.state.Operations)
	if r.Selection != nil {
		if c, ok := m.capabilities[r.Selection.Provider]; ok {
			r.ToolBridgeEnabled, r.ToolBridgeStatus = c.Tools, c.ToolBridge
			r.ToolCapability = "text-only"
			if c.Tools {
				r.ToolCapability = "Unreal tools via SDK MCP"
				if r.ToolBridgeMode == "structured" {
					r.ToolCapability = "Unreal tools via structured actions"
				}
			}
		}
	}
	if m.state.ContextPackage != nil {
		d := *m.state.ContextPackage
		r.ContextPackage = &d
	}
	if m.historyMore {
		r.Partial = true
		r.Usage.Partial = true
		r.Errors.CrashesKnown = false
	}
	return r
}

func (m *Model) setCapabilities(capabilities map[string]modelcatalog.Capabilities) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capabilities = make(map[string]modelcatalog.Capabilities, len(capabilities))
	for id, c := range capabilities {
		m.capabilities[id] = c
	}
}
func (m *Model) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.Lines = append([]string(nil), s.Lines...)
	s.Entries = cloneEntries(s.Entries)
	s.Operations = append([]operation.Operation(nil), s.Operations...)
	if s.Progress != nil {
		p := *s.Progress
		s.Progress = &p
	}
	if s.ContextPackage != nil {
		d := *s.ContextPackage
		s.ContextPackage = &d
	}
	return s
}
func (m *Model) status(s string, connected bool) {
	m.mu.Lock()
	m.state.Status = s
	if !connected && (m.state.Connected || m.state.OfflineSince.IsZero()) {
		m.state.OfflineSince = time.Now()
	}
	m.state.Connected = connected
	if !connected {
		m.state.Progress = nil
	}
	m.mu.Unlock()
}
func (m *Model) apply(v host.View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &m.state
	if m.analysis != nil {
		m.analysis.Created(v.Session.Session.CreatedAt)
	}
	m.historyMore = v.History.More
	var responseTurn session.TurnID
	responseSeen := false
	if v.Generation != s.Generation {
		m.completedEpoch = 0
		m.hasCompletedEpoch = false
		m.seenEpoch = 0
		s.Progress = nil
	}
	for _, item := range v.History.Items {
		if item.Sequence <= s.After {
			continue
		}
		if item.Sequence != s.After+1 {
			return errors.New("history gap")
		}
		s.After = item.Sequence
		if m.analysis == nil {
			m.analysis = analysis.New(s.ID)
		}
		m.analysis.Apply(item)
		if item.Kind == sessionstore.ItemFork {
			s.Selection, s.PendingSelection = nil, nil
		}
		s.LatestKind, s.LatestAt = item.Kind, item.RecordedAt
		switch data := item.Data.(type) {
		case session.Turn:
			s.TurnID = data.ID
		case sessionstore.HostRecord:
			switch data.Kind {
			case "configuration":
				if s.Selection == nil {
					s.Selection = sessionstore.SelectionFromConfiguration(data.Configuration)
				}
			case sessionstore.HostRuntimeSelection:
				s.PendingSelection = data.Selection
			case sessionstore.HostRuntimeApplied:
				s.Selection = data.Selection
				if s.PendingSelection != nil && data.Selection != nil && s.PendingSelection.Revision <= data.Selection.Revision {
					s.PendingSelection = nil
				}
			}
		case sessionstore.ModelResponse:
			responseTurn = data.TurnID
			responseSeen = true
			s.WaitingForModel = false
			if s.Progress != nil && s.Progress.TurnID == data.TurnID {
				m.completedEpoch = s.Progress.Epoch
				m.hasCompletedEpoch = true
				s.Progress = nil
			}
		case inbox.Input:
			if data.Kind == inbox.InputExternal {
				s.WaitingForModel = true
			}
			if control, err := data.DecodeControlMessage(); err == nil && control.Mode == inbox.UpdateSettings && s.Selection != nil {
				choice := *s.Selection
				choice.Effort = control.Parameters.(inbox.Settings).ReasoningEffort
				s.Selection = &choice
			}
		case sessionstore.ToolCallStatus:
			updateReceipts(s.Entries, data, item.RecordedAt)
			for _, op := range data.Operations {
				if terminalStatus(op.Status) {
					s.WaitingForModel = true
				}
			}
		}
		s.Entries = append(s.Entries, entriesFor(item)...)
		for _, line := range itemLines(item) {
			s.Lines = append(s.Lines, clipBytes(line, 4096))
		}
		var dropped bool
		s.Entries, dropped = boundEntries(s.Entries, transcriptDisplayEntries)
		s.OlderDropped = s.OlderDropped || dropped
		// Display is a bounded window. Canonical history remains available by paging.
		if len(s.Lines) > 1024 {
			s.Lines = append([]string(nil), s.Lines[len(s.Lines)-1024:]...)
		}
	}
	if v.Generation != s.Generation || v.Revision >= s.Revision {
		s.Generation = v.Generation
		s.Revision = v.Revision
		s.Running = v.Running
		s.Operations = v.Operations
		if v.Context != nil {
			d := *v.Context
			s.ContextPackage = &d
		} else {
			s.ContextPackage = nil
		}
		m.setProgress(v.Progress)
		if responseSeen && v.Progress != nil && v.Progress.Done && v.Progress.TurnID == responseTurn {
			m.completedEpoch = v.Progress.Epoch
			m.hasCompletedEpoch = true
			s.Progress = nil
		}
	}
	s.Connected = true
	s.OfflineSince = time.Time{}
	s.Failure = SafeText(v.Failure)
	s.Status = "connected"
	if !s.Running {
		s.Status = "stopped; /resume continues this session"
	}
	if v.Failure != "" {
		s.Status = "session failed: " + v.Failure
	}
	return nil
}
func (m *Model) progress(e host.Event) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &m.state
	if e.Generation != s.Generation || e.Revision != s.Revision+1 {
		return false
	}
	s.Revision = e.Revision
	m.setProgress(e.Progress)
	return true
}

// Progress epochs/attempts are local projection guards, not canonical state.
func (m *Model) setProgress(p *host.Progress) {
	if p == nil {
		m.state.Progress = nil
		return
	}
	if p.Epoch < m.seenEpoch || m.hasCompletedEpoch && p.Epoch <= m.completedEpoch {
		return
	}
	if old := m.state.Progress; old != nil && p.Epoch == old.Epoch && p.Attempt < old.Attempt {
		return
	}
	copy := *p
	copy.Text = clipBytes(copy.Text, 64<<10)
	m.state.Progress = &copy
	m.seenEpoch = p.Epoch
}
func itemLines(item host.HistoryItem) []string {
	switch item.Kind {
	case sessionstore.ItemInput:
		input := item.Data.(inbox.Input)
		if input.Kind == inbox.InputExternal {
			var text string
			if json.Unmarshal(input.Payload, &text) == nil {
				return []string{"you> " + text}
			}
		}
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse).Response
		var result []string
		for _, item := range response.Output {
			if item.Type == llm.ItemMessage {
				result = append(result, "agent> "+item.Data.(llm.Message).Text)
			}
		}
		if response.Failure != nil {
			result = append(result, "error> "+response.Failure.Message)
		}
		return result
	}
	return nil
}

// Watch batches canonical history refreshes independently of rendering. Events
// are hints; history cursors and latest operation snapshots are authoritative.
func Watch(ctx context.Context, r Reader, m *Model, changed func()) error {
	for ctx.Err() == nil {
		s := m.Snapshot()
		sub, err := r.Subscribe(ctx, s.ID, s.After, 128, 32)
		if err != nil {
			m.status("disconnected; reconnecting", false)
			changed()
			if !pause(ctx) {
				break
			}
			continue
		}
		err = m.apply(sub.Initial)
		for more := sub.Initial.History.More; err == nil && more; {
			s = m.Snapshot()
			v, e := r.Inspect(ctx, s.ID, s.After, 128)
			err = e
			if err == nil {
				err = m.apply(v)
				more = v.History.More
			}
		}
		changed()
		if err != nil {
			sub.Cancel()
			m.status("resync required", false)
			changed()
			if !pause(ctx) {
				break
			}
			continue
		}
		reconnect := false
		for !reconnect {
			select {
			case <-ctx.Done():
				sub.Cancel()
				return ctx.Err()
			case e, ok := <-sub.Events:
				if !ok || e.Kind == "gap" || e.Kind == "disconnected" {
					if ok || m.Snapshot().Running {
						m.status("gap/disconnect; resyncing", false)
					}
					changed()
					reconnect = true
					continue
				}
				s = m.Snapshot()
				if e.Generation == s.Generation && e.Revision <= s.Revision {
					continue
				}
				if e.Kind == "progress" && m.progress(e) {
					changed()
					continue
				}
				for {
					s = m.Snapshot()
					v, e := r.Inspect(ctx, s.ID, s.After, 128)
					if e != nil {
						err = e
						break
					}
					if err = m.apply(v); err != nil || !v.History.More {
						break
					}
				}
				changed()
				if err != nil {
					m.status("disconnected; reconnecting", false)
					changed()
					reconnect = true
				}
			}
		}
		sub.Cancel()
		if !pause(ctx) {
			break
		}
	}
	return ctx.Err()
}
func pause(ctx context.Context) bool {
	t := time.NewTimer(250 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// SafeText strips terminal control sequences' introducers/control bytes. All
// provider/tool/user text is data, never a terminal command.
func SafeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}
func Wrap(s string, width int) []string {
	width = max(1, width)
	var result []string
	for _, line := range strings.Split(SafeText(s), "\n") {
		var b strings.Builder
		cells := 0
		g := uniseg.NewGraphemes(strings.ReplaceAll(line, "\t", "    "))
		for g.Next() {
			text := g.Str()
			w := g.Width()
			if cells+w > width && cells > 0 {
				result = append(result, b.String())
				b.Reset()
				cells = 0
			}
			if w > width {
				text = "�"
				w = 1
			}
			b.WriteString(text)
			cells += w
		}
		result = append(result, b.String())
	}
	return result
}
