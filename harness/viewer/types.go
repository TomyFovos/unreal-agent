// Package viewer projects canonical Host history for a UI. It never schedules
// work, acquires session writer locks, or writes a Store.
package viewer

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"time"
)

var (
	ErrResync      = errors.New("viewer requires a fresh canonical snapshot")
	ErrStale       = errors.New("viewer received an obsolete generation or revision")
	ErrInvalidPage = errors.New("viewer received a noncontiguous history page")
	ErrUnavailable = errors.New("viewer control is unavailable")
)

// Reader methods are read-only: observing an unopened child must never resume it.
type Reader interface {
	Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)
	Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error)
}

// DecodeChild validates a supported canonical plan, never guesses from tool names.
type Child struct {
	ID    session.ID
	Label string
}
type ChildDecoder func(session.ID, operation.Operation) (Child, bool, error)

// Finish is the child's explicit canonical result, never an inferred outcome.
// OperationID is the child Finish operation, not the parent SubagentStart ID.
type Finish struct {
	OperationID  operation.ID
	Status       string
	Summary      string
	ChangedFiles []string
	Tests        []string
	Blockers     []string
	RecordedAt   time.Time
}
type FinishDecoder func(host.HistoryItem) (*Finish, error)
type Options struct {
	RecentLimit  int // bounded transcript cache; default 128, maximum 4096
	DecodeChild  ChildDecoder
	DecodeFinish FinishDecoder
}
type Liveness string

const (
	RuntimeUnknown Liveness = "unknown"
	RuntimeRunning Liveness = "running"
	RuntimeStopped Liveness = "stopped"
)

// Input includes cached/cache-write input; Output includes reasoning tokens.
type Usage struct {
	Known           bool
	Partial         bool
	Input           int64
	CachedInput     int64
	CacheWriteInput int64
	Output          int64
	Reasoning       int64
	Responses       int
}
type Duration struct {
	Known bool
	Value time.Duration
}
type OperationRow struct {
	ID      operation.ID
	Tool    string
	Type    operation.Type
	Status  operation.Status
	Elapsed Duration
}
type Row struct {
	ProjectInstructions   *projectinstructions.Metadata
	ID                    session.ID
	ParentID              session.ID
	ParentOperationID     operation.ID
	ParentOperationStatus operation.Status
	Label                 string
	Depth                 int
	Runtime               Liveness
	Generation            string
	Finish                *Finish
	Failure               string
	Activity              string
	Usage                 Usage
	// Elapsed is wall time since canonical creation, not CPU or active time.
	Elapsed     Duration
	Operations  []OperationRow
	Cursor      sessionstore.Sequence
	More        bool
	NeedsResync bool
	Problem     string
}
type Detail struct {
	Row         Row
	Recent      []host.HistoryItem
	RecentAfter sessionstore.Sequence // use Reader.Inspect for older pages
}

// Retry a transport failure with the same caller-owned InputID.
type ControlRequest struct {
	ParentID         session.ID
	ParentGeneration string
	OperationID      operation.ID
	ChildID          session.ID
	InputID          inbox.ID
	Text             string
}
type Controls interface {
	SteerChild(context.Context, ControlRequest) (host.Receipt, error)
	CancelChild(context.Context, ControlRequest) (host.Receipt, error)
	ResumeChild(context.Context, ControlRequest) error
}
