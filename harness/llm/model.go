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
	// ToolObservations describe work inside a tool call. They are history only.
	ToolObservations []ToolObservation `json:",omitzero"`
	// ParentCallID scopes a Host-executed call to another tool's conversation.
	ParentCallID string `json:",omitzero"`
	// OutputLimit identifies output from a capped provider request inside a
	// response that can also contain successful requests (Claude Code).
	OutputLimit bool `json:",omitzero"`
	// ClaudeResumeBoundary marks the verified prefix in Claude Code's private
	// transcript, including hook inputs followed by an empty model response.
	ClaudeResumeBoundary bool `json:"-"`
}

// ToolObservation records a message, reasoning, tool call or result inside a
// tool's own conversation. The observations are not part of model input.
type ToolObservation struct {
	ParentCallID string
	Type         ItemType
	Model        string `json:",omitzero"`
	CallID       string `json:",omitzero"`
	Tool         string `json:",omitzero"`
	Text         string `json:",omitzero"`
	IsError      bool   `json:",omitzero"`
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
	// PublicHistory uses a history of public messages and tool receipts.
	PublicHistory bool `json:"-"`
	// Window identifies a three-layer projection for an adapter that resumes
	// private conversations. DeliveredItems is the prefix already acknowledged
	// by that conversation, including any copied state.
	Window *ConversationWindow `json:"-"`
}

type ConversationWindow struct {
	Boundary       string
	DeliveredItems int
}

type StopReason string

const (
	StopComplete        StopReason = "complete"
	StopMaxOutputTokens StopReason = "max_output_tokens"
	StopRefused         StopReason = "refused"
	// StopWindowBoundary hands a completed tool batch back to the Coordinator
	// so it can start the next request without recording an extra model reply.
	StopWindowBoundary StopReason = "window_boundary"
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
	// FirstRequestUsage isolates the first model call when one adapter
	// invocation can make several calls. The Host records its window cache.
	FirstRequestUsage *Usage `json:"-"`
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
