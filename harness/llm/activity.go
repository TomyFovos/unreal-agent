package llm

import "time"

const (
	ActivityModelFailure      = "model_failure"
	ActivityFastUnsupported   = "fast_unsupported"
	ActivitySubscriptionUsage = "subscription_usage"
)

// ActivityRecord contains timing and runtime metadata, never commands, file
// contents, provider credentials, or tool arguments. Missing fields in older
// histories remain unknown.
type ActivityRecord struct {
	Kind         string
	ID           string              `json:",omitzero"`
	StartedAt    time.Time           `json:",omitzero"`
	EndedAt      time.Time           `json:",omitzero"`
	RetryAt      time.Time           `json:",omitzero"`
	Revision     string              `json:",omitzero"`
	Modified     bool                `json:",omitzero"`
	Provider     string              `json:",omitzero"`
	Model        string              `json:",omitzero"`
	Effort       string              `json:",omitzero"`
	Template     string              `json:",omitzero"`
	Layer        string              `json:",omitzero"` // child_start: role group at start, independent of provider
	Tool         string              `json:",omitzero"`
	Reason       string              `json:",omitzero"`
	Usage        *Usage              `json:",omitzero"`
	RequestUsage *RequestUsage       `json:",omitzero"`
	Subscription []SubscriptionUsage `json:",omitzero"`
	Fast         *bool               `json:",omitzero"`
	Redaction    *ContextRedaction   `json:",omitzero"`
}

// RequestUsage holds counters whose presence is known independently. Input is
// the total input; Uncached is populated only when the provider sends it.
type RequestUsage struct {
	Input, Read, Write, Uncached, Output *int64 `json:",omitzero"`
}

// SubscriptionUsage is an account allowance snapshot. Percent and reset are
// provider fields; they do not describe token usage or billed cost.
type SubscriptionUsage struct {
	Window, Status string    `json:",omitzero"`
	UsedPercent    *float64  `json:",omitzero"`
	WindowMinutes  *int64    `json:",omitzero"`
	ResetsAt       time.Time `json:",omitzero"`
}

// ContextRedaction measures public payload bytes, excluding provider replay
// metadata and framing. It never contains the payload itself.
type ContextRedaction struct {
	Reason      string `json:"reason"`
	CallID      string `json:"call_id,omitzero"`
	BeforeBytes int64  `json:"before_bytes"`
	AfterBytes  int64  `json:"after_bytes"`
}
