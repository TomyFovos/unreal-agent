package contextstate

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

const OperationType = "session_context"
const QuestionMarker = "【要確認】"

type Action string

const (
	Show       Action = "StateShow"
	Decide     Action = "StateDecide"
	SetPlan    Action = "StatePlan"
	AddRule    Action = "StateRuleAdd"
	Accept     Action = "StateAccept"
	Reject     Action = "StateReject"
	AskUser    Action = "StateQuestion"
	AnswerUser Action = "StateAnswer"
	ShowRecord Action = "RecordShow"
	FindRecord Action = "RecordFind"
	// Import is an operator command of the Session CLI, not a model tool:
	// it copies another Session's recorded state into this Session.
	Import Action = "StateImport"
)

// ImportNote carries another Session's current rules, decisions, open plan
// steps and open questions. IDs stay as recorded there; the Host prefixes
// them with From, and locators keep pointing to the original Session.
type ImportNote struct {
	From      string     `json:"from"`
	Rules     []Rule     `json:"rules,omitempty"`
	Decisions []Decision `json:"decisions,omitempty"`
	Plan      []PlanStep `json:"plan,omitempty"`
	Questions []Question `json:"questions,omitempty"`
}

// ImportFrom selects what an import carries from a replayed State.
func ImportFrom(from string, s State) ImportNote {
	note := ImportNote{From: from, Plan: slices.Clone(s.Plan.Steps)}
	for _, id := range slices.Sorted(maps.Keys(s.Rules)) {
		note.Rules = append(note.Rules, s.Rules[id])
	}
	for _, id := range slices.Sorted(maps.Keys(s.Decisions)) {
		note.Decisions = append(note.Decisions, s.Decisions[id])
	}
	for _, id := range slices.Sorted(maps.Keys(s.QuestionsToUser.Current)) {
		if q := s.QuestionsToUser.Current[id]; q.Status == QuestionOpen {
			note.Questions = append(note.Questions, q)
		}
	}
	return note
}

func Actions() []Action {
	return []Action{Show, Decide, SetPlan, AddRule, Accept, Reject, AskUser, AnswerUser, ShowRecord, FindRecord}
}

func IsTool(name string) bool { return slices.Contains(Actions(), Action(name)) }

type Command struct {
	Action Action
	Args   jsontext.Value
}

type DecisionNote struct {
	ID       string                `json:"id,omitempty"`
	Decision string                `json:"decision"`
	Why      string                `json:"why,omitempty"`
	Basis    []record.Locator      `json:"basis,omitempty"`
	Rejected []RejectedAlternative `json:"rejected,omitempty"`
	Plan     []string              `json:"plan,omitempty"`
	Children []string              `json:"children,omitempty"`
	Requests []string              `json:"requests,omitempty"`
}

type PlanNote struct {
	ID        string     `json:"id"`
	Position  *int       `json:"position,omitempty"`
	Text      *string    `json:"text,omitempty"`
	Status    PlanStatus `json:"status,omitempty"`
	Owner     *string    `json:"owner,omitempty"`
	DependsOn []string   `json:"depends_on,omitzero"`
	Decisions []string   `json:"decisions,omitzero"`
}

type RuleNote struct {
	ID      string         `json:"id,omitempty"`
	Excerpt string         `json:"excerpt"`
	Source  record.Locator `json:"source"`
}

type ReviewNote struct {
	Handle string `json:"handle"`
	Why    string `json:"why,omitempty"`
}

type QuestionNote struct {
	ID     string         `json:"id,omitempty"`
	Text   string         `json:"text,omitempty"`
	Source record.Locator `json:"source,omitempty"`
}

type AnswerNote struct {
	ID     string         `json:"id"`
	Source record.Locator `json:"source"`
}

type RecordSelection struct {
	Locator   record.Locator `json:"locator"`
	PageBytes int64          `json:"page_bytes,omitempty"`
}

type RecordSearch struct {
	Kind   record.Kind `json:"kind,omitempty"`
	Handle string      `json:"handle,omitempty"`
	From   time.Time   `json:"from,omitzero"`
	Before time.Time   `json:"before,omitzero"`
	Text   string      `json:"text,omitempty"`
}

type Result struct {
	State   *State         `json:",omitzero"`
	Page    *record.Page   `json:",omitzero"`
	Matches []record.Match `json:",omitzero"`
	ID      string         `json:"id,omitempty"`
	Locator record.Locator `json:"locator,omitempty"`
}

type recordBody struct {
	Text   *string `json:"text,omitempty"`
	Base64 []byte  `json:"base64,omitempty"`
}

func body(raw []byte) recordBody {
	if !utf8.Valid(raw) {
		return recordBody{Base64: raw}
	}
	text := string(raw)
	return recordBody{Text: &text}
}

// JSON formats tool output and CLI JSON. Binary evidence has a lossless base64
// representation. Text, including its line endings, is available directly.
func (r Result) JSON(action Action, options ...json.Options) ([]byte, error) {
	switch action {
	case Show:
		return json.Marshal(r.State, options...)
	case ShowRecord:
		return json.Marshal(struct {
			recordBody
			Continue record.Locator `json:"continue,omitempty"`
		}{body(r.Page.Bytes), r.Page.Continue}, options...)
	case FindRecord:
		type match struct {
			Locator record.Locator `json:"locator"`
			Kind    record.Kind    `json:"kind"`
			Handle  string         `json:"handle,omitempty"`
			At      time.Time      `json:"at,omitzero"`
			Excerpt recordBody     `json:"excerpt"`
		}
		items := make([]match, 0, len(r.Matches))
		for _, m := range r.Matches {
			items = append(items, match{m.Locator, m.Kind, m.Handle, m.At, body(m.Excerpt)})
		}
		return json.Marshal(items, options...)
	default:
		return json.Marshal(r, options...)
	}
}
