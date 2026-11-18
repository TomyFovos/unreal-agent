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
	Responses  *ItemEncoding `json:"-"`
	// OutputLimit identifies output from a capped provider request inside a
	// response that can also contain successful requests (Claude Code).
	OutputLimit bool `json:",omitzero"`
	// ClaudeResumeBoundary marks the verified prefix in Claude Code's private
	// transcript, including hook inputs followed by an empty model response.
	ClaudeResumeBoundary bool `json:"-"`
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
	// Incomplete calls are recorded as evidence when a response hits its cap.
	Incomplete bool `json:",omitzero"`
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
	Fast            bool `json:",omitzero"`
}

type ReasoningEffort string

const (
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
	// Kept for reading legacy records; Unreal treats ultra as max.
	ReasoningEffortUltra ReasoningEffort = "ultra"
)

// Normalize resolves legacy settings without rewriting immutable records.
func (effort ReasoningEffort) Normalize() ReasoningEffort {
	if effort == ReasoningEffortUltra {
		return ReasoningEffortMax
	}
	return effort
}

func (effort ReasoningEffort) Valid() bool {
	switch effort.Normalize() {
	case ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax:
		return true
	default:
		return false
	}
}

type Request struct {
	Model Model
	Input []Item
	Tools []Tool
}

type StopReason string

const (
	StopComplete        StopReason = "complete"
	StopMaxOutputTokens StopReason = "max_output_tokens"
	StopRefused         StopReason = "refused"
)

// IsOutputLimit recognizes provider stop and finish reasons for exhausted output.
func IsOutputLimit(reason string) bool {
	switch reason {
	case string(StopMaxOutputTokens), "max_tokens", "length":
		return true
	default:
		return false
	}
}

type Response struct {
	ID         string
	Provider   string `json:",omitzero"`
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
	Provider              string `json:",omitzero"`
	AuthMethod            string `json:",omitzero"` // api_key, oauth, or the selected subscription transport
	Fast                  *bool  `json:",omitzero"` // whether the Codex request carried the Fast tier
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
