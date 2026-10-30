package llm

import "time"

// ConversationRecord describes a provider's private conversation. Unknown
// post-compaction sizes stay unknown until the provider reports usage again.
type ConversationRecord struct {
	Provider        string
	InputTokens     int64
	InputKnown      bool
	BeforeTokens    int64     `json:",omitzero"`
	Compacted       bool      `json:",omitzero"`
	Interrupted     bool      `json:",omitzero"`
	Error           string    `json:",omitzero"`
	Phase           string    `json:",omitzero"`
	StartedAt       time.Time `json:",omitzero"`
	EndedAt         time.Time `json:",omitzero"`
	InterruptReason string    `json:",omitzero"`
	Usage           *Usage    `json:",omitzero"`
}
