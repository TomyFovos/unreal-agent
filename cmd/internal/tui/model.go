// Package tui is a disposable projection of Host history and snapshots. It never
// opens a Session store and never performs model/tool work.
package tui

import (
	"context"
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
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/displaytext"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

type Reader interface {
	Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)
	Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error)
}
type Snapshot struct {
	Attention         []string
	HistoryWorkPrefix *host.HistoryWork
	LastUserAt        time.Time
	// Version changes for every local projection update, including permissions,
	// connection state and progress that need not change the Host revision.
	Version                           uint64
	Permissions                       permission.ModeSnapshot
	ID                                session.ID
	Generation                        string
	Revision                          uint64
	After                             sessionstore.Sequence
	Running, Connected                bool
	Status                            string
	Failure                           string
	OfflineSince                      time.Time
	Entries                           []Entry
	OlderDropped                      bool
	LazyHistory                       bool
	HistoryFirst                      sessionstore.Sequence
	HistoryBackground, TailBackground bool
	LatestKind                        sessionstore.ItemKind
	LatestAt                          time.Time
	Operations                        []operation.Operation
	Progress                          *host.Progress
	LastGeneration                    *llm.GenerationStatistics
	SpeedHistory                      []llm.GenerationStatistics
	CacheLatest, CacheTotal           viewer.Usage
	WaitingForModel                   bool
	MainInterrupted                   bool
	Selection                         *sessionstore.RuntimeSelection
	PendingSelection                  *sessionstore.RuntimeSelection
	ContextPackage                    *contextengine.Diagnostics
	TurnID                            session.TurnID // observed canonical turn, for frontend topology
	Workspace                         string         // immutable Session configuration, for filename completion only
}
type Model struct {
	mu                    sync.Mutex
	state                 Snapshot
	completedEpoch        uint64
	hasCompletedEpoch     bool
	seenEpoch             uint64
	analysis              *analysis.Accumulator
	historyMore           bool
	analysisFull          bool
	createdAt             time.Time
	olderMu               sync.Mutex
	diagnostics           []analysis.Event
	capabilities          map[string]modelcatalog.Capabilities
	speedHistory          [8]llm.GenerationStatistics
	speedNext, speedCount int
}

func NewModel(id session.ID) *Model {
	return &Model{state: Snapshot{ID: id, Status: "connecting", Version: 1}, analysis: analysis.New(id), analysisFull: true}
}

func (m *Model) Version() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Version
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
				r.ToolCapability = "Tools available"
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
	if m.historyMore || !m.analysisFull {
		r.Partial = true
		r.Usage.Partial = true
		r.Errors.CrashesKnown = false
	}
	if m.state.Failure != "" && (len(m.diagnostics) == 0 || m.diagnostics[len(m.diagnostics)-1].Name != safeDiagnostic(m.state.Failure)) {
		r.Errors.Recent = append(r.Errors.Recent, analysis.Event{At: m.state.LatestAt, Kind: "session", Name: safeDiagnostic(m.state.Failure)})
	}
	r.Errors.Recent = append(r.Errors.Recent, m.diagnostics...)
	for _, pending := range m.state.Permissions.Pending {
		r.Timeline = append(r.Timeline, analysis.Event{Kind: "pending approval", Name: safeDiagnostic(pending.Tool + " · Operation " + pending.ID)})
	}
	return r
}

func safeDiagnostic(text string) string {
	text = secretguard.Redact(SafeText(text))
	if secretguard.Sensitive(text) {
		return "[protected]"
	}
	return clipBytes(text, 2048)
}

func (m *Model) diagnostic(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagnostics = append(m.diagnostics, analysis.Event{At: time.Now(), Kind: "connection", Name: safeDiagnostic(err.Error())})
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
	s.SpeedHistory = make([]llm.GenerationStatistics, m.speedCount)
	for i := range s.SpeedHistory {
		s.SpeedHistory[i] = m.speedHistory[(m.speedNext-m.speedCount+i+len(m.speedHistory))%len(m.speedHistory)]
	}
	s.Entries = cloneEntries(s.Entries)
	s.Attention = append([]string(nil), s.Attention...)
	if s.HistoryWorkPrefix != nil {
		prefix := *s.HistoryWorkPrefix
		prefix.Turns = append([]session.TurnID(nil), prefix.Turns...)
		prefix.Reports = append([]session.ID(nil), prefix.Reports...)
		s.HistoryWorkPrefix = &prefix
	}

	s.Operations = append([]operation.Operation(nil), s.Operations...)
	s.Permissions.Pending = append([]permission.Pending(nil), s.Permissions.Pending...)
	if s.Progress != nil {
		p := *s.Progress
		if p.Statistics != nil {
			copy := *p.Statistics
			p.Statistics = &copy
		}
		s.Progress = &p
	}
	if s.LastGeneration != nil {
		copy := *s.LastGeneration
		s.LastGeneration = &copy
	}
	if s.ContextPackage != nil {
		d := *s.ContextPackage
		s.ContextPackage = &d
	}
	return s
}

func (m *Model) watchSnapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	return Snapshot{ID: s.ID, Generation: s.Generation, Revision: s.Revision, After: s.After, Connected: s.Connected, Running: s.Running}
}
func (m *Model) status(s string, connected bool) {
	m.mu.Lock()
	m.state.Version++
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
	m.state.Version++
	s := &m.state
	if m.analysis != nil {
		m.createdAt = v.Session.Session.CreatedAt
		m.analysis.Created(m.createdAt)
	}
	m.historyMore = v.History.More
	var responseTurn session.TurnID
	responseSeen := false
	if v.Generation != s.Generation {
		m.completedEpoch = 0
		m.hasCompletedEpoch = false
		m.seenEpoch = 0
		s.Progress = nil
		s.Permissions = permission.ModeSnapshot{}
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
			s.Workspace = ""
		}
		s.LatestKind, s.LatestAt = item.Kind, item.RecordedAt
		switch data := item.Data.(type) {
		case session.Turn:
			s.TurnID = data.ID
		case sessionstore.HostRecord:
			switch data.Kind {
			case sessionstore.HostContext:
				if data.Context != nil && data.Context.Usage != nil {
					s.CacheLatest = viewer.Usage{}
					viewer.AddUsage(&s.CacheLatest, *data.Context.Usage)
					viewer.AddUsage(&s.CacheTotal, *data.Context.Usage)
				}
			case "configuration":
				s.Workspace = fileWorkspace(data.Configuration)
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
			s.CacheLatest = viewer.Usage{}
			viewer.AddUsage(&s.CacheLatest, data.Response.Usage)
			viewer.AddUsage(&s.CacheTotal, data.Response.Usage)
			if data.Response.Generation != nil && data.Response.Generation.Valid() {
				copy := *data.Response.Generation
				s.LastGeneration = &copy
				m.recordGeneration(copy)
			} else if data.Response.Failure == nil {
				s.LastGeneration = &llm.GenerationStatistics{Model: data.Response.Model, Tokens: llm.ReportedTokens(data.Response.Usage)}
				m.recordGeneration(*s.LastGeneration)
			}
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
				s.LastUserAt = item.RecordedAt
				s.WaitingForModel = true
				s.MainInterrupted = false
			}
			if control, err := data.DecodeControlMessage(); err == nil && (control.Mode == inbox.InterruptAgent || control.Mode == inbox.StopHard) {
				s.MainInterrupted, s.WaitingForModel = true, false
				if s.Progress != nil {
					m.completedEpoch, m.hasCompletedEpoch = s.Progress.Epoch, true
				}
				s.Progress = nil
			}
			if control, err := data.DecodeControlMessage(); err == nil && control.Mode == inbox.UpdateSettings && s.Selection != nil {
				choice := *s.Selection
				choice.Effort = control.Parameters.(inbox.Settings).ReasoningEffort
				s.Selection = &choice
			}
		case sessionstore.ToolCallStatus:
			updateReceipts(s.Entries, data, item.RecordedAt)
			for _, op := range data.Operations {
				if terminalStatus(op.Status) && !s.MainInterrupted {
					s.WaitingForModel = true
				}
			}
		}
		if s.HistoryFirst == 0 {
			s.HistoryFirst = item.Sequence
		}
		if input, ok := item.Data.(inbox.Input); ok {
			if input.Kind == inbox.InputExternal {
				s.TailBackground = false
			}
			if input.Kind == inbox.InputPeer {
				s.TailBackground = true
			}
		}
		newEntries := entriesFor(item)
		for _, entry := range newEntries {
			if entry.Role == "you" {
				s.Attention = nil
			}
			if entry.Role == "agent" {
				if message, ok := displaytext.Attention(entry.Text); ok {
					s.Attention = append(s.Attention, message)
				}
			}
		}
		s.Entries = append(s.Entries, newEntries...)
		var dropped bool
		if !s.LazyHistory {
			s.Entries, dropped = boundEntries(s.Entries, transcriptDisplayEntries)
		}
		s.OlderDropped = s.OlderDropped || dropped
	}
	if v.Generation != s.Generation || v.Revision >= s.Revision {
		s.Generation = v.Generation
		s.Revision = v.Revision
		s.Running = v.Running
		s.Operations = v.Operations
		if v.Permissions.Revision >= s.Permissions.Revision || v.Generation != s.Generation {
			s.Permissions = v.Permissions
		}
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
	if v.Failure != "" && SafeText(v.Failure) != s.Failure {
		m.diagnostics = append(m.diagnostics, analysis.Event{At: s.LatestAt, Kind: "session", Name: safeDiagnostic(v.Failure)})
	}
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

// recordGeneration runs only for a new canonical response. Equal measurements
// from distinct generations are still distinct samples; replayed pages add none.
func (m *Model) recordGeneration(stats llm.GenerationStatistics) {
	m.speedHistory[m.speedNext] = stats
	m.speedNext = (m.speedNext + 1) % len(m.speedHistory)
	m.speedCount = min(m.speedCount+1, len(m.speedHistory))
}

func (m *Model) progress(e host.Event) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &m.state
	if e.Generation != s.Generation || e.Revision != s.Revision+1 {
		return false
	}
	s.Revision = e.Revision
	s.Version++
	m.setProgress(e.Progress)
	return true
}

// Progress epochs/attempts are local projection guards, not canonical state.
func (m *Model) setProgress(p *host.Progress) {
	if m.state.MainInterrupted {
		m.state.Progress = nil
		return
	}
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
	if p.Statistics != nil {
		stats := *p.Statistics
		copy.Statistics = &stats
	}
	copy.Text = clipBytes(copy.Text, 64<<10)
	m.state.Progress = &copy
	m.seenEpoch = p.Epoch
}

// Watch batches canonical history refreshes independently of rendering. Events
// are hints; history cursors and latest operation snapshots are authoritative.
func Watch(ctx context.Context, r Reader, m *Model, changed func()) error {
	for ctx.Err() == nil {
		s := m.watchSnapshot()
		if s.Connected && !s.Running && s.Generation != "" {
			if reader, ok := r.(viewer.VersionReader); ok {
				if version, err := reader.Version(ctx, s.ID); err == nil && version.Generation == s.Generation && version.Revision == s.Revision {
					if !pause(ctx) {
						break
					}
					continue
				}
			}
		}
		// Bootstrap from the tail once. Subscribe from that exact canonical
		// cursor afterwards, so writes between these two calls are replayed.
		if s.Generation == "" {
			if backward, ok := r.(BackwardReader); ok {
				v, err := backward.InspectBefore(ctx, s.ID, 0, historyPageItems)
				if err == nil {
					err = m.seedTail(v)
				}
				if err != nil {
					m.status("disconnected; reconnecting", false)
					changed()
					if !pause(ctx) {
						break
					}
					continue
				}
				s = m.watchSnapshot()
			}
		}
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
			s = m.watchSnapshot()
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
					if ok || m.watchSnapshot().Running {
						m.status("gap/disconnect; resyncing", false)
					}
					changed()
					reconnect = true
					continue
				}
				s = m.watchSnapshot()
				if e.Generation == s.Generation && e.Revision <= s.Revision {
					continue
				}
				if e.Kind == "progress" && m.progress(e) {
					changed()
					continue
				}
				for {
					s = m.watchSnapshot()
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
