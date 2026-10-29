// Package contextengine indexes canonical history in recording order.
package contextengine

import (
	"errors"
	"fmt"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

const Version = 1

type Class string

const (
	Pin  Class = "PIN"
	Keep Class = "KEEP"
	Omit Class = "OMIT_FROM_REQUEST"
)

type Kind string

const (
	UserMessage  Kind = "user_message"
	ModelMessage Kind = "model_message"
	ToolRequest  Kind = "tool_request"
	ToolResult   Kind = "tool_result"
	ChildResult  Kind = "child_result"
	ErrorUnit    Kind = "error"
)

// HistoryRef identifies original evidence, not a provider continuation handle.
type HistoryRef struct {
	SourceType  string
	Sequence    sessionstore.Sequence
	TurnID      string    `json:",omitzero"`
	OperationID string    `json:",omitzero"`
	ChildID     string    `json:",omitzero"`
	RecordedAt  time.Time `json:",omitzero"`
	OutputIndex int       `json:",omitzero"`
}

type Unit struct {
	ID       string
	Kind     Kind
	Class    Class
	Source   HistoryRef
	Origin   string
	Group    string
	Item     llm.Item `json:"-"` // allowlisted public data only; never persisted
	Required bool
}

// Runtime omits credentials, endpoint, account identity and runtime bindings.
type Runtime struct {
	Provider, Model, Effort string
	Revision                uint64
	ContextWindow           int64 `json:",omitzero"`
	Tools                   bool
}

type OperationState struct {
	ID, Type, Tool, Status  string
	ChildID                 string `json:",omitzero"`
	Provider, Model, Effort string `json:",omitzero"`
	Outcome                 string `json:",omitzero"`
}

type Constraints struct {
	ToolPolicy, Filesystem, Network, Process string
	AllowedTools                             []string `json:",omitzero"`
}

type State struct {
	Runtime                  Runtime
	Constraints              Constraints
	Operations               []OperationState
	ProjectInstructionsBound bool
	ThroughSequence          sessionstore.Sequence
	ActiveOperations         int
}

type Budget struct {
	Window, ResponseReserve, SchemaReserve, ProtocolReserve, Input int64
	WindowSource                                                   string // catalog or conservative-fallback
}

// Diagnostics contains no unit bodies, arguments, search terms or private IDs.
type Diagnostics struct {
	Version                                   int
	Provider, Model, Effort                   string
	RuntimeRevision                           uint64
	ThroughSequence                           sessionstore.Sequence
	CanonicalUnits, Recent, Omitted, Excluded int
	EstimatedInputTokens                      int64
	Budget                                    Budget
	Utilization                               float64
	Measurement, Cache                        string
}

type Selected struct {
	Unit   Unit
	Reason string // history or pruned
}

// Package is a request-local value. Copies own their slices and public payloads.
// Request construction does not change the indexed source units.
type Package struct {
	Version         int
	Runtime         Runtime
	Instructions    llm.Item `json:"-"`
	Selected        []Selected
	IncludedUnitIDs []string
	EstimatedTokens int64
	Budget          Budget
}

type Error struct{ Code string }

func (e *Error) Error() string { return "context: " + e.Code }
func IsBudgetError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "required_context_exceeds_budget"
}

// Config controls view compaction. Retired selection fields remain decodable
// for existing configurations; they no longer affect derivation.
type Config struct {
	Version                                                       int    `json:",omitzero"`
	Mode                                                          string `json:",omitzero"`
	FallbackWindow, InputBudget, ResponseReserve, ProtocolReserve int64  `json:",omitzero"`
	RecentReserve                                                 int64  `json:",omitzero"`
	RetrievalReserve                                              int64  `json:",omitzero"`
	RetrievalLimit, CheckpointThreshold                           int    `json:",omitzero"`
	LargeToolTokens                                               int64  `json:",omitzero"`
	ToolResultKeepTokens, ToolResultMinPruneTokens                int64  `json:",omitzero"`
	ClaudeCompactThresholdTokens                                  int64  `json:",omitzero"`
}

func (c Config) Resolve() (Config, error) {
	if c.ClaudeCompactThresholdTokens <= 0 {
		c.ClaudeCompactThresholdTokens = 200_000
	}
	if c.Version == 0 {
		c.Version = Version
	}
	if c.Mode == "" {
		c.Mode = "bounded"
	}
	if c.Version != Version || c.Mode != "bounded" && c.Mode != "legacy" {
		return c, &Error{"unsupported_strategy"}
	}
	if c.FallbackWindow < 0 || c.InputBudget < 0 || c.ResponseReserve < 0 || c.ProtocolReserve < 0 || c.RecentReserve < 0 || c.RetrievalReserve < 0 || c.RetrievalLimit < 0 || c.CheckpointThreshold < 0 || c.LargeToolTokens < 0 || c.ToolResultKeepTokens < 0 || c.ToolResultMinPruneTokens < 0 {
		return c, &Error{"invalid_configuration"}
	}
	// Like Codex, a request may fill the model's context window: the input
	// budget is the window less the reserves unless configured lower. An
	// unknown window (Codex through Docker Sandboxes lists none) falls back to
	// 768 KiB of estimated bytes: bytes overstate tokens about threefold, so
	// it stays within the smallest current window, 272k tokens.
	if c.FallbackWindow == 0 {
		c.FallbackWindow = 768 << 10
	}
	if c.InputBudget == 0 {
		c.InputBudget = 16 << 20
	}
	if c.ResponseReserve == 0 {
		c.ResponseReserve = 4096
	}
	if c.ProtocolReserve == 0 {
		c.ProtocolReserve = 1024
	}
	if c.LargeToolTokens == 0 {
		c.LargeToolTokens = 2048
	}
	if c.ToolResultKeepTokens == 0 {
		c.ToolResultKeepTokens = 15000
	}
	if c.ToolResultMinPruneTokens == 0 {
		c.ToolResultMinPruneTokens = 15000
	}
	if c.FallbackWindow <= c.ResponseReserve+c.ProtocolReserve {
		return c, &Error{"invalid_budget"}
	}
	return c, nil
}

func (c Config) budget(r Runtime, schemas int64) (Budget, error) {
	b := Budget{Window: r.ContextWindow, WindowSource: "catalog", ResponseReserve: c.ResponseReserve, ProtocolReserve: c.ProtocolReserve, SchemaReserve: schemas}
	if b.Window <= 0 {
		b.Window, b.WindowSource = c.FallbackWindow, "conservative-fallback"
	}
	b.Input = min(c.InputBudget, b.Window-b.ResponseReserve-b.ProtocolReserve-b.SchemaReserve)
	if b.Input <= 0 {
		return b, &Error{"no_input_budget"}
	}
	return b, nil
}

func ReferenceText(u Unit) string {
	return fmt.Sprintf("[Canonical %s retained; history sequence %d, source unit %s. Body not included; do not infer its contents.]", u.Kind, u.Source.Sequence, u.ID)
}

// BudgetFor uses existing window/reserve defaults, including legacy configuration.
func (c Config) BudgetFor(r Runtime, schemas int64) (Budget, error) { return c.budget(r, schemas) }

func (c Config) Threshold(r Runtime, maximumOutput int64) int64 {
	w := r.ContextWindow
	if w <= 0 {
		w = c.FallbackWindow
	}
	if maximumOutput <= 0 {
		maximumOutput = c.ResponseReserve
	}
	return min(w*8/10, w-maximumOutput-64000, c.InputBudget)
}
