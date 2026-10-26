package sessionstore

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// HistoryView is a bounded-lifetime, owner-scoped canonical read boundary.
// Close releases all derived history before reconciliation or canonical writes.
// Implementations must not mix pages from different boundaries or expose mutable
// aliases across calls. It conveys no Operation execution authority.
type HistoryView interface {
	Items(context.Context, session.ID, Sequence, int) (Page, error)
	Close() error
}

// HistoryViewStore is optional: existing Store implementations keep their
// existing behavior. A Host implementation holds its Session writer serialization
// until the view closes. The Coordinator uses it only while loading history.
type HistoryViewStore interface {
	OpenHistoryView(context.Context, session.ID) (HistoryView, error)
}
