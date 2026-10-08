package viewer

import (
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// PanelSnapshot exposes existing frontend projection data to terminal layouts.
// It adds no Host/gateway fields, canonical records, controls, or ownership.
type PanelSnapshot struct {
	ParentID, Selected session.ID
	Rows               []Row
	Transcript         *host.HistoryPage
	TranscriptAfter    sessionstore.Sequence
	Problem            string
	Retry              bool
}

func (p *Panel) Snapshot(now time.Time) PanelSnapshot {
	s := PanelSnapshot{ParentID: p.parent, Selected: p.client.Model.Selected(), Rows: p.client.Model.Rows(now)}
	p.mu.Lock()
	defer p.mu.Unlock()
	s.Problem = p.problem
	s.Retry = p.retry != nil
	if p.transcript != nil && p.transcriptID == s.Selected {
		page, err := copyValue(*p.transcript)
		if err == nil {
			s.Transcript = &page
			s.TranscriptAfter = p.transcriptAfter
		}
	}
	return s
}
