package tui

import (
	"context"
	"errors"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// This is a terminal-local page. It never changes the live subscription cursor,
// canonical state, or the Context Engine's provider-facing history selection.
type transcriptPage struct {
	Entries      []Entry
	First, Last  sessionstore.Sequence
	Older, Newer bool
}

func readHistory(ctx context.Context, r Reader, id session.ID, after, through sessionstore.Sequence, visit func(host.HistoryItem) bool) error {
	for after < through {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := r.Inspect(ctx, id, after, 128)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("canonical history unavailable; reconnect and retry")
		}
		progress := false
		for _, item := range v.History.Items {
			if item.Sequence <= after {
				continue
			}
			if item.Sequence > through {
				if after < through {
					return errors.New("canonical history incomplete; reconnect and retry")
				}
				return nil
			}
			if item.Sequence != after+1 {
				return errors.New("canonical history incomplete; reconnect and retry")
			}
			after = item.Sequence
			progress = true
			if !visit(item) {
				return nil
			}
		}
		if after == through {
			return nil
		}
		if !progress || !v.History.More {
			return errors.New("canonical history incomplete; reconnect and retry")
		}
	}
	return nil
}

// Legacy readers retain a bounded transcript page through forward Inspect.
// Production terminals extend the live projection with LoadOlder instead.
func loadTranscriptPage(ctx context.Context, r Reader, id session.ID, boundary, through sessionstore.Sequence, older bool) (*transcriptPage, error) {
	p := &transcriptPage{}
	after, end := boundary, through
	if older {
		after, end = 0, boundary-1
	}
	err := readHistory(ctx, r, id, after, end, func(item host.HistoryItem) bool {
		entries := entriesFor(item)
		update := item.Data
		if status, ok := update.(sessionstore.ToolCallStatus); ok {
			updateReceipts(p.Entries, status, item.RecordedAt)
		}
		if len(entries) == 0 {
			p.Last = item.Sequence
			return true
		}
		if older {
			p.Last = item.Sequence
			p.Entries = append(p.Entries, entries...)
			p.Entries, _ = boundEntries(p.Entries, 256)
			return true
		}
		candidate := append(append([]Entry(nil), p.Entries...), entries...)
		bounded, dropped := boundEntries(candidate, 256)
		if dropped && len(p.Entries) > 0 {
			return false
		}
		p.Entries = bounded
		p.Last = item.Sequence
		return len(p.Entries) < 256
	})
	if err != nil {
		return nil, err
	}
	if len(p.Entries) == 0 {
		if !older {
			p.Last = through
			return p, nil
		}
		return nil, errors.New("no earlier/later transcript messages")
	}
	p.First = p.Entries[0].Sequence
	p.Older, p.Newer = p.First > 1, p.Last < through
	return p, nil
}

// 128 canonical items typically include several events per exchange: a few
// screenfuls of messages, while keeping large-session attach work independent
// of the total history. The same window is fetched for each upward request.
const historyPageItems = 128

type BackwardReader interface {
	InspectBefore(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)
}

func (m *Model) seedTail(v host.View) error {
	m.mu.Lock()
	if m.state.Generation != "" {
		m.mu.Unlock()
		return nil
	}
	s := &m.state
	s.LazyHistory = true
	s.HistoryBackground, s.TailBackground = v.History.Background, v.History.Background
	if len(v.History.Items) > 0 {
		s.HistoryFirst = v.History.Items[0].Sequence
		s.After = s.HistoryFirst - 1
	}
	s.OlderDropped = v.History.Older
	s.HistoryWorkPrefix = v.History.WorkPrefix
	m.analysisFull = !v.History.Older
	m.mu.Unlock()
	if err := m.apply(v); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	applyHistoryReceipts(s.Entries, v.HistoryReceipts)
	if h := v.HistoryState; h != nil {
		s.Selection, s.PendingSelection = h.Selection, h.PendingSelection
		s.Workspace = fileWorkspace(h.Configuration)
		s.TurnID, s.LatestKind, s.LatestAt = h.TurnID, h.LatestKind, h.LatestAt
		s.LastUserAt = h.LastUserAt
		s.Attention = append([]string(nil), h.Attention...)
		s.WaitingForModel, s.MainInterrupted, s.TailBackground = h.WaitingForModel, h.MainInterrupted, h.Background
		s.CacheLatest, s.CacheTotal = viewer.Usage{}, viewer.Usage{}
		if h.UsageResponses > 0 {
			viewer.AddUsage(&s.CacheLatest, h.CacheLatest)
			viewer.AddUsage(&s.CacheTotal, h.CacheTotal)
			s.CacheTotal.Responses = h.UsageResponses
			s.CacheTotal.Partial = h.UsagePartial
		}
		m.speedNext, m.speedCount = 0, 0
		for _, stats := range h.Generations {
			m.recordGeneration(stats)
			copy := stats
			s.LastGeneration = &copy
		}
	}
	return nil
}

// LoadOlder only extends the terminal projection; the live After/Revision and
// analysis accumulator remain owned by the forward synchronization path.
func (m *Model) LoadOlder(ctx context.Context, r BackwardReader) error {
	m.olderMu.Lock()
	defer m.olderMu.Unlock()
	m.mu.Lock()
	first, id, generation := m.state.HistoryFirst, m.state.ID, m.state.Generation
	older := m.state.OlderDropped
	m.mu.Unlock()
	if !older || first <= 1 {
		return nil
	}
	v, err := r.InspectBefore(ctx, id, first, historyPageItems)
	if err != nil {
		return err
	}
	var entries []Entry
	for _, item := range v.History.Items {
		if item.Sequence >= first {
			continue
		}
		if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
			updateReceipts(entries, status, item.RecordedAt)
		}
		entries = append(entries, entriesFor(item)...)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Generation != generation || m.state.HistoryFirst != first {
		return nil
	}
	applyHistoryReceipts(entries, v.HistoryReceipts)
	m.state.Entries = append(entries, m.state.Entries...)
	if len(v.History.Items) > 0 {
		m.state.HistoryFirst = v.History.Items[0].Sequence
	}
	m.state.HistoryBackground = v.History.Background
	m.state.HistoryWorkPrefix = v.History.WorkPrefix
	m.state.OlderDropped = v.History.Older
	m.state.Version++
	return nil
}

// EnsureAnalysis reconstructs full statistics on demand, then catches up any
// concurrent appends before installing the accumulator under the model lock.
func (m *Model) EnsureAnalysis(ctx context.Context, r Reader) error {
	m.mu.Lock()
	full, id := m.analysisFull, m.state.ID
	m.mu.Unlock()
	if full {
		return nil
	}
	accumulator := analysis.New(id)
	var after sessionstore.Sequence
	for {
		m.mu.Lock()
		through := m.state.After
		m.mu.Unlock()
		if err := readHistory(ctx, r, id, after, through, func(item host.HistoryItem) bool { accumulator.Apply(item); after = item.Sequence; return true }); err != nil {
			return err
		}
		m.mu.Lock()
		if m.state.After == after {
			accumulator.Created(m.createdAt)
			m.analysis, m.analysisFull = accumulator, true
			m.state.Version++
			m.mu.Unlock()
			return nil
		}
		m.mu.Unlock()
	}
}

func applyHistoryReceipts(entries []Entry, receipts []host.HistoryReceipt) {
	for _, receipt := range receipts {
		updateReceipts(entries, receipt.Status, receipt.At)
		for i := range entries {
			for j := range entries[i].Calls {
				call := &entries[i].Calls[j]
				if call.CallID == receipt.Status.CallID && call.StartedAt.IsZero() {
					call.StartedAt = receipt.StartedAt
				}
			}
		}
	}
}
