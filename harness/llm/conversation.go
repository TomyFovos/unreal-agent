package llm

// ConversationRecord describes a provider's private conversation. Unknown
// post-compaction sizes stay unknown until the provider reports usage again.
type ConversationRecord struct {
	Provider     string
	InputTokens  int64
	InputKnown   bool
	BeforeTokens int64  `json:",omitzero"`
	Compacted    bool   `json:",omitzero"`
	Interrupted  bool   `json:",omitzero"`
	Error        string `json:",omitzero"`
}
