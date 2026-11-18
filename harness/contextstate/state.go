// Package contextstate defines the provider-neutral current state of a Session.
package contextstate

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

const Version uint32 = 1

// State has the thirteen L2 slots. A zero-valued field is unknown until a
// recorded event supplies it. State contains neither prompts nor thinking.
type State struct {
	Version         uint32              `json:"version"`
	Session         Session             `json:"session"`
	Requests        Entries[Request]    `json:"requests"`
	Rules           map[string]Rule     `json:"rules"`
	Plan            Plan                `json:"plan"`
	Decisions       map[string]Decision `json:"decisions"`
	Children        Children            `json:"children"`
	Approvals       Entries[Approval]   `json:"approvals"`
	Waiting         Entries[Wait]       `json:"waiting"`
	QuestionsToUser Entries[Question]   `json:"questions_to_user"`
	Artifacts       Entries[Artifact]   `json:"artifacts"`
	Issues          Entries[Issue]      `json:"issues"`
	RecentEvents    []RecentEvent       `json:"recent_events"`
	Metrics         Metrics             `json:"metrics"`
	Extra           jsontext.Value      `json:",embed"`
}

type stateJSON State

// MarshalJSON orders map keys so replay has a single byte representation.
func (s State) MarshalJSON() ([]byte, error) {
	if s.Version == 0 {
		s.Version = Version
	}
	return json.Marshal(stateJSON(s), json.Deterministic(true), json.FormatNilMapAsNull(true), json.FormatNilSliceAsNull(true))
}

// Additive members from newer State schemas survive a decode/encode cycle.
func (s *State) UnmarshalJSON(data []byte) error {
	var v stateJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	if v.Version == 0 {
		return fmt.Errorf("missing context state schema version")
	}
	*s = State(v)
	return nil
}

type Evidence struct {
	Sources []record.Locator `json:"sources,omitempty"`
}

type Processed struct {
	Count   uint64                      `json:"count"`
	Sources map[record.Locator]struct{} `json:"sources,omitempty"`
}

type Entries[T any] struct {
	Current   map[string]T `json:"current,omitempty"`
	Processed Processed    `json:"processed"`
}

type Session struct {
	Evidence
	ID             string    `json:"id,omitempty"`
	Started        time.Time `json:"started,omitzero"`
	PermissionMode string    `json:"permission_mode,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Model          string    `json:"model,omitempty"`
	Effort         string    `json:"effort,omitempty"`
	Directory      string    `json:"directory,omitempty"`
	RepositoryHEAD string    `json:"repository_head,omitempty"`
}

type RequestOrigin string

const (
	HumanInput RequestOrigin = "human"
	MixedInput RequestOrigin = "mixed"
	PeerInput  RequestOrigin = "peer"
)

type RequestStatus string

const (
	RequestActive RequestStatus = "active"
	RequestDone   RequestStatus = "done"
	RequestParked RequestStatus = "parked"
)

type Request struct {
	Evidence
	ID        string           `json:"id"`
	TS        time.Time        `json:"ts"`
	FirstLine string           `json:"first_line"`
	Chars     int              `json:"chars"`
	Locator   record.Locator   `json:"locator"`
	Origin    RequestOrigin    `json:"origin,omitempty"`
	Original  record.Locator   `json:"original,omitempty"`
	Aliases   []record.Locator `json:"aliases,omitempty"`
	Status    RequestStatus    `json:"status,omitempty"`
	Text      string           `json:"text,omitempty"`
}

type Rule struct {
	Evidence
	ID      string         `json:"id"`
	Excerpt string         `json:"excerpt"`
	Locator record.Locator `json:"locator"`
}

type PlanStatus string

const (
	PlanTodo    PlanStatus = "todo"
	PlanDoing   PlanStatus = "doing"
	PlanDone    PlanStatus = "done"
	PlanDropped PlanStatus = "dropped"
)

type Plan struct {
	Steps     []PlanStep `json:"steps,omitempty"`
	Processed Processed  `json:"processed"`
}

type PlanStep struct {
	Evidence
	ID        string     `json:"id"`
	Position  int        `json:"position"`
	Text      string     `json:"text"`
	Status    PlanStatus `json:"status"`
	Owner     string     `json:"owner"`
	DependsOn []string   `json:"depends_on,omitempty"`
	Decisions []string   `json:"decisions,omitempty"`
}

type RejectedAlternative struct {
	Alternative string `json:"alternative"`
	Reason      string `json:"reason"`
}

type Decision struct {
	Evidence
	ID       string                `json:"id"`
	TS       time.Time             `json:"ts"`
	Decision string                `json:"decision"`
	Reason   string                `json:"reason,omitempty"`
	Basis    []record.Locator      `json:"basis,omitempty"`
	Rejected []RejectedAlternative `json:"rejected,omitempty"`
	Plan     []string              `json:"plan,omitempty"`
	Children []string              `json:"children,omitempty"`
	Requests []string              `json:"requests,omitempty"`
	Locator  record.Locator        `json:"locator"`
}

type ChildStatus string

const (
	ChildRunning         ChildStatus = "running"
	ChildWaitingApproval ChildStatus = "waiting_approval"
	ChildAsked           ChildStatus = "asked"
	ChildFinished        ChildStatus = "finished"
	ChildFailed          ChildStatus = "failed"
	ChildCancelled       ChildStatus = "cancelled"
)

type Acceptance string

const (
	AcceptPending  Acceptance = "pending"
	AcceptAccepted Acceptance = "accepted"
	AcceptRejected Acceptance = "rejected"
)

type Ending string

const (
	EndedByFinish            Ending = "finish"
	EndedByCoordinatorCancel Ending = "coordinator_cancel"
	EndedByUserCancel        Ending = "user_cancel"
	EndedByFailure           Ending = "failed"
)

type Child struct {
	Evidence
	Handle              string           `json:"handle"`
	Title               string           `json:"title"`
	Purpose             string           `json:"purpose,omitempty"`
	Outputs             string           `json:"outputs,omitempty"`
	Verification        string           `json:"verification,omitempty"`
	AcceptanceCriteria  string           `json:"acceptance_criteria,omitempty"`
	Template            string           `json:"template"`
	Model               string           `json:"model"`
	Effort              string           `json:"effort"`
	State               ChildStatus      `json:"state"`
	Started             time.Time        `json:"started,omitzero"`
	Ended               time.Time        `json:"ended,omitzero"`
	LastEvent           record.Locator   `json:"last_event,omitempty"`
	ResultFirstLine     string           `json:"result_first_line,omitempty"`
	Report              record.Locator   `json:"report,omitempty"`
	Sends               uint64           `json:"sends"`
	UnansweredQuestions []record.Locator `json:"unanswered_questions,omitempty"`
	Accept              Acceptance       `json:"accept"`
	Rework              uint64           `json:"rework"`
	EndedBy             Ending           `json:"ended_by,omitempty"`
}

type Children struct {
	Entries[Child]
	ByEnding []ChildCount `json:"by_ending,omitempty"`
}

type ChildCount struct {
	Template string `json:"template"`
	Model    string `json:"model"`
	EndedBy  Ending `json:"ended_by"`
	Count    uint64 `json:"count"`
}

type Approval struct {
	Evidence
	ID       string         `json:"id"`
	Handle   string         `json:"handle,omitempty"`
	Request  string         `json:"request"`
	Decision string         `json:"decision,omitempty"`
	TS       time.Time      `json:"ts"`
	Locator  record.Locator `json:"locator"`
}

type WaitKind string

const (
	WaitUserAnswer      WaitKind = "user_answer"
	WaitChildCompletion WaitKind = "child_completion"
	WaitApproval        WaitKind = "approval"
	WaitOperation       WaitKind = "operation"
	WaitUsageLimit      WaitKind = "usage_limit_wait"
	WaitParentFailure   WaitKind = "model_failure"
	WaitExternal        WaitKind = "external"
)

type Wait struct {
	Evidence
	ID       string         `json:"id"`
	Kind     WaitKind       `json:"kind"`
	TS       time.Time      `json:"ts"`
	Handle   string         `json:"handle,omitempty"`
	Question record.Locator `json:"question,omitempty"`
	Text     string         `json:"text,omitempty"`
	RetryAt  time.Time      `json:"retry_at,omitzero"`
}

type QuestionStatus string

const (
	QuestionOpen     QuestionStatus = "open"
	QuestionAnswered QuestionStatus = "answered"
)

type Question struct {
	Evidence
	ID        string         `json:"id"`
	TS        time.Time      `json:"ts"`
	FirstLine string         `json:"first_line"`
	Status    QuestionStatus `json:"status"`
	Locator   record.Locator `json:"locator"`
	Answer    record.Locator `json:"answer,omitempty"`
}

type Artifact struct {
	Evidence
	Path       string         `json:"path"`
	LastWriter string         `json:"last_writer"`
	TS         time.Time      `json:"ts"`
	Locator    record.Locator `json:"locator"`
	Note       string         `json:"note,omitempty"`
}

type IssueStatus string

const (
	IssueOpen   IssueStatus = "open"
	IssueClosed IssueStatus = "closed"
)

type Issue struct {
	Evidence
	ID      string         `json:"id"`
	Text    string         `json:"text"`
	Status  IssueStatus    `json:"status"`
	Locator record.Locator `json:"locator"`
}

type RecentEvent struct {
	Evidence
	TS        time.Time      `json:"ts"`
	Kind      string         `json:"kind"`
	FirstLine string         `json:"first_line"`
	Locator   record.Locator `json:"locator"`
	Handle    string         `json:"handle,omitempty"`
	RuleID    string         `json:"rule_id,omitempty"`
}

type Usage struct {
	Input      *uint64 `json:"input,omitempty"`
	Output     *uint64 `json:"output,omitempty"`
	CacheRead  *uint64 `json:"cache_read,omitempty"`
	CacheWrite *uint64 `json:"cache_write,omitempty"`
}

type Boundary struct {
	Evidence
	TS           time.Time      `json:"ts"`
	WindowTokens *uint64        `json:"window_tokens,omitempty"`
	Locator      record.Locator `json:"locator"`
}

type BoundaryRecords struct {
	Notifications *uint64 `json:"notifications,omitempty"`
	Summaries     *uint64 `json:"summaries,omitempty"`
	Markers       *uint64 `json:"markers,omitempty"`
}

type Metrics struct {
	Evidence
	ChildrenByState map[ChildStatus]uint64 `json:"children_by_state,omitempty"`
	ModelRequests   *uint64                `json:"model_requests,omitempty"`
	Usage           *Usage                 `json:"usage,omitempty"`
	ListPriceUSD    *float64               `json:"list_price_usd,omitempty"`
	ElapsedMillis   *uint64                `json:"elapsed_millis,omitempty"`
	LastBoundary    *Boundary              `json:"last_boundary,omitempty"`
	RawBoundaries   *BoundaryRecords       `json:"raw_boundaries,omitempty"`
	Rebuilds        *uint64                `json:"rebuilds,omitempty"`
}
