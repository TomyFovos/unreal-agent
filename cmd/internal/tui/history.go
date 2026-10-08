package tui

import (
	"context"
	"errors"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
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

// Backward Inspect is not part of the gateway protocol. Read the existing
// forward pages, retaining only a bounded predecessor window. This is done on
// a user's page request, never on each frame or on the model request path.
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
