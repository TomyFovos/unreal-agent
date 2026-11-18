package contextstate

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

type EventKind string

const (
	SessionChanged   EventKind = "session"
	RequestChanged   EventKind = "request"
	RuleRecorded     EventKind = "rule"
	PlanChanged      EventKind = "plan"
	DecisionRecorded EventKind = "decision"
	ChildChanged     EventKind = "child"
	ApprovalChanged  EventKind = "approval"
	WaitChanged      EventKind = "wait"
	QuestionChanged  EventKind = "question_to_user"
	ArtifactChanged  EventKind = "artifact"
	IssueChanged     EventKind = "issue"
	RecentChanged    EventKind = "recent_events"
	RecentObserved   EventKind = "recent_event"
	MetricsChanged   EventKind = "metrics"
	BoundaryObserved EventKind = "boundary"
)

// Event is the canonical, versioned update envelope. Unknown versions or
// kinds retain their payload in the log and have no effect on known slots.
type Event struct {
	Version uint32           `json:"version"`
	ID      string           `json:"id"`
	Kind    EventKind        `json:"kind"`
	Record  record.Locator   `json:"record,omitempty"`
	Sources []record.Locator `json:"sources,omitempty"`
	Data    jsontext.Value   `json:"data"`
	Extra   jsontext.Value   `json:",embed"`
}

func NewEvent(id string, kind EventKind, value any, sources ...record.Locator) (Event, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return Event{}, err
	}
	e := Event{Version: Version, ID: id, Kind: kind, Sources: append([]record.Locator(nil), sources...), Data: data}
	return e, e.Validate()
}

func (e Event) Validate() error {
	if e.Version == 0 || e.ID == "" || e.Kind == "" || e.Data.Kind() == jsontext.KindNull || !e.Data.IsValid() {
		return fmt.Errorf("invalid context state event envelope")
	}
	return nil
}

type ChildUpdate struct {
	Child
	CancelRecorded bool   `json:"cancel_recorded,omitempty"`
	Why            string `json:"why,omitempty"`
}

type WaitUpdate struct {
	Wait
	Resolved bool `json:"resolved,omitempty"`
}

type ApprovalUpdate struct {
	Approval
	AlreadyClosed bool `json:"already_closed,omitempty"`
}

type QuestionUpdate struct {
	Question
	Text string `json:"text,omitempty"`
}
