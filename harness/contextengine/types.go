// Package contextengine selects bounded, provider-neutral projections. History
// remains the authority; all engine state can be discarded and replayed.
package contextengine

import (
	"errors"
	"fmt"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

const Version = 1

type Class string

const (
	Pin       Class = "PIN"
	Keep      Class = "KEEP"
	Reference Class = "REFERENCE"
	Omit      Class = "OMIT_FROM_REQUEST"
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
// Byte offsets refer to a text output when a large receipt is retrieved in parts.
type HistoryRef struct {
	SourceType         string
	Sequence           sessionstore.Sequence
	TurnID             string    `json:",omitzero"`
	OperationID        string    `json:",omitzero"`
	ChildID            string    `json:",omitzero"`
	RecordedAt         time.Time `json:",omitzero"`
	OutputIndex        int       `json:",omitzero"`
	StartByte, EndByte int       `json:",omitzero"`
}

type Unit struct {
	ID            string
	Kind          Kind
	Class         Class
	Source        HistoryRef
	Origin        string
	Group         string
	Item          llm.Item `json:"-"` // allowlisted public data only; never persisted
	Tokens        int64
	Required      bool
	Staged        bool
	RetrievalOnly bool
	Fingerprint   string
	ParentID      string `json:",omitzero"`
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
	Runtime                                                                         Runtime
	Constraints                                                                     Constraints
	Operations                                                                      []OperationState
	ProjectInstructionsBound                                                        bool
	ProjectInstructions                                                             *projectinstructions.Metadata `json:",omitzero"`
	Task                                                                            *HistoryRef                   `json:",omitzero"`
	CurrentRequest                                                                  *HistoryRef                   `json:",omitzero"`
	ThroughSequence, CheckpointBoundary                                             sessionstore.Sequence
	ActiveOperations, FailedOperations, ObservedChildren, ReferencedOperationStates int
}

// Checkpoint is machine state at a canonical boundary, never a prose summary.
type Checkpoint struct {
	Version                       int
	FromSequence, ThroughSequence sessionstore.Sequence
	State                         State
	SourceRefs                    []HistoryRef
	CreatedAt                     time.Time
}

type Budget struct {
	Window, ResponseReserve, SchemaReserve, ProtocolReserve, Input int64
	WindowSource                                                   string // catalog or conservative-fallback
}

// Diagnostics contains no unit bodies, arguments, search terms or private IDs.
type Diagnostics struct {
	Version                                                          int
	Provider, Model, Effort                                          string
	RuntimeRevision                                                  uint64
	ThroughSequence, CheckpointBoundary                              sessionstore.Sequence
	CheckpointVersion                                                int
	CanonicalUnits, Recent, Retrieved, Referenced, Omitted, Excluded int
	Retrieval                                                        RetrievalDiagnostics
	EstimatedInputTokens                                             int64
	TransportReserve                                                 int64 `json:",omitzero"`
	Budget                                                           Budget
	Utilization                                                      float64
	RetrievalMicros                                                  int64
	Measurement                                                      string // estimated; never actual provider usage
	Cache                                                            string
}

// Counts describe selection, never query text or source contents. Retrieved in
// Diagnostics is the final package count, not the number of search hits.
type RetrievalDiagnostics struct {
	Candidates, Selected                                              int
	SkippedRecent, SkippedDuplicate, SkippedBudget, SkippedIneligible int
}

type Selected struct {
	Unit          Unit
	Reason        string // pin, recent, retrieved, referenced
	ReferenceOnly bool
}

// Package is a request-local value. Copies own their slices and public payloads.
// Latency lives separately in Diagnostics and cannot influence selection.
type Package struct {
	Version                            int
	Runtime                            Runtime
	Instructions                       llm.Item `json:"-"`
	State                              State
	Checkpoint                         Checkpoint
	Selected                           []Selected
	IncludedUnitIDs, ReferencedUnitIDs []string
	EstimatedTokens                    int64
	Budget                             Budget
}

type Error struct{ Code string }

func (e *Error) Error() string { return "context: " + e.Code }
func IsBudgetError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "required_context_exceeds_budget"
}

// Config is a versioned strategy configuration, not canonical Session memory.
// Zero fields use v1 defaults. Mode=legacy is an explicit comparison boundary.
type Config struct {
	Version                                                       int    `json:",omitzero"`
	Mode                                                          string `json:",omitzero"`
	FallbackWindow, InputBudget, ResponseReserve, ProtocolReserve int64  `json:",omitzero"`
	RecentReserve                                                 int64  `json:",omitzero"`
	RetrievalReserve                                              int64  `json:",omitzero"`
	RetrievalLimit, CheckpointThreshold                           int    `json:",omitzero"`
	LargeToolTokens                                               int64  `json:",omitzero"`
}

func (c Config) Resolve() (Config, error) {
	if c.Version == 0 {
		c.Version = Version
	}
	if c.Mode == "" {
		c.Mode = "bounded"
	}
	if c.Version != Version || c.Mode != "bounded" && c.Mode != "legacy" {
		return c, &Error{"unsupported_strategy"}
	}
	if c.FallbackWindow < 0 || c.InputBudget < 0 || c.ResponseReserve < 0 || c.ProtocolReserve < 0 || c.RecentReserve < 0 || c.RetrievalReserve < 0 || c.RetrievalLimit < 0 || c.CheckpointThreshold < 0 || c.LargeToolTokens < 0 {
		return c, &Error{"invalid_configuration"}
	}
	if c.FallbackWindow == 0 {
		c.FallbackWindow = 32768
	}
	if c.InputBudget == 0 {
		c.InputBudget = 24576
	}
	if c.ResponseReserve == 0 {
		c.ResponseReserve = 4096
	}
	if c.ProtocolReserve == 0 {
		c.ProtocolReserve = 1024
	}
	if c.RecentReserve == 0 {
		c.RecentReserve = 12000
	}
	if c.RetrievalReserve == 0 {
		c.RetrievalReserve = min(2048, max(1, c.InputBudget/4))
	}
	if c.RetrievalLimit == 0 {
		c.RetrievalLimit = 8
	}
	if c.CheckpointThreshold == 0 {
		c.CheckpointThreshold = 128
	}
	if c.LargeToolTokens == 0 {
		c.LargeToolTokens = 2048
	}
	if c.FallbackWindow > 16<<20 || c.InputBudget > 16<<20 || c.ResponseReserve > 16<<20 || c.ProtocolReserve > 16<<20 || c.RecentReserve > 16<<20 || c.RetrievalReserve > 16<<20 || c.LargeToolTokens > 16<<20 || c.RetrievalLimit > 128 || c.CheckpointThreshold > 65536 {
		return c, &Error{"invalid_configuration"}
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
