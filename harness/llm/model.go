package llm

import "encoding/json/jsontext"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

type ItemType string

const (
	ItemMessage    ItemType = "message"
	ItemToolCall   ItemType = "tool_call"
	ItemToolResult ItemType = "tool_result"
	ItemReasoning  ItemType = "reasoning"
)

type Item struct {
	ProviderID string
	Type       ItemType
	Data       any
	// Derived portable receipt provenance has no canonical/JSON input path.
	receiptProjection *receiptProjection `json:"-"`
}

type Message struct {
	Role  Role
	Text  string
	Phase string
}

type ToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

type ToolResultKind string

const (
	ToolResultText  ToolResultKind = "text"
	ToolResultImage ToolResultKind = "image"
)

type ToolResultOutput struct {
	Kind  ToolResultKind
	Value string
}

type ToolResult struct {
	CallID string
	Output []ToolResultOutput
	// State is Host-derived request projection data, never accepted from JSON
	// or persisted as a second receipt. Resume derives it from canonical facts.
	State ToolResultState `json:"-"`
}

type ToolResultState uint8

const (
	ToolResultUnknown ToolResultState = iota
	ToolResultPending
	ToolResultSucceeded
	ToolResultFailed
)

// Raw is the provider's verbatim reasoning item. A provider may attach state to
// it that the harness cannot reconstruct, such as encrypted reasoning content, so
// adapters replay Raw unchanged instead of re-encoding Summary.
type Reasoning struct {
	Summary []string       `json:",omitzero"`
	Raw     jsontext.Value `json:",omitzero"`
}

type ToolType string

const (
	ToolFunction ToolType = "function"
	ToolHosted   ToolType = "hosted"
)

type Tool struct {
	Type        ToolType
	Name        string
	Description string
	Parameters  map[string]any
}

type Model struct {
	ID              string
	MaxOutputTokens *int64
	ReasoningEffort ReasoningEffort
}

type ReasoningEffort string

const (
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
	// Ultra is the Codex catalog's top level for models such as GPT-6.1-Sol.
	ReasoningEffortUltra ReasoningEffort = "ultra"
)

func (effort ReasoningEffort) Valid() bool {
	switch effort {
	case ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax, ReasoningEffortUltra:
		return true
	default:
		return false
	}
}

type Request struct {
	Model Model
	Input []Item
	Tools []Tool
	// Bound only after Builder selection/rendering, never serialized to a model.
	receiptProjections *receiptProjectionBindings `json:"-"`
}

type StopReason string

const (
	StopComplete        StopReason = "complete"
	StopMaxOutputTokens StopReason = "max_output_tokens"
	StopRefused         StopReason = "refused"
)

type Response struct {
	ID         string
	Model      string `json:",omitzero"`
	Stop       StopReason
	Output     []Item `json:",omitzero"`
	Usage      Usage
	Failure    *Failure
	Generation *GenerationStatistics `json:",omitzero"`
}

// InputTokens includes CachedInputTokens and CacheWriteInputTokens.
// OutputTokens includes ReasoningTokens.
type Usage struct {
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningTokens       int64
	// Unknown marks fields a provider did not report. Their numeric zero is not
	// a measured zero. Empty preserves the existing adapters' usage contract.
	Unknown []UsageField   `json:",omitzero"`
	Raw     jsontext.Value `json:",omitzero"`
}

type UsageField string

const (
	UsageInput           UsageField = "input"
	UsageCachedInput     UsageField = "cached_input"
	UsageCacheWriteInput UsageField = "cache_write_input"
	UsageOutput          UsageField = "output"
	UsageReasoning       UsageField = "reasoning"
)

type Failure struct {
	Code    string
	Message string
}
