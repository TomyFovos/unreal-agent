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
	Version            uint64
	ParentID, Selected session.ID
	Rows               []Row
	Transcript         *host.HistoryPage
	TranscriptAfter    sessionstore.Sequence
	Problem            string
	Retry              bool
}

func (p *Panel) Snapshot(now time.Time) PanelSnapshot {
	rows, version := p.client.Model.RowsVersion(now)
	s := PanelSnapshot{ParentID: p.parent, Selected: p.client.Model.Selected(), Rows: rows}
	p.mu.Lock()
	defer p.mu.Unlock()
	s.Problem = p.problem
	s.Retry = p.retry != nil
	if p.snapshotVersion == 0 || p.snapshotModelVersion != version || p.snapshotSelected != s.Selected || p.snapshotProblem != s.Problem || p.snapshotRetry != s.Retry || p.snapshotTranscript != p.transcript {
		p.snapshotVersion++
		p.snapshotModelVersion, p.snapshotSelected, p.snapshotProblem, p.snapshotRetry, p.snapshotTranscript = version, s.Selected, s.Problem, s.Retry, p.transcript
	}
	s.Version = p.snapshotVersion
	if p.transcript != nil && p.transcriptID == s.Selected {
		page, err := copyValue(*p.transcript)
		if err == nil {
			s.Transcript = &page
			s.TranscriptAfter = p.transcriptAfter
		}
	}
	return s
}
