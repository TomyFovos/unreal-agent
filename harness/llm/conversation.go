package llm

import (
	"strings"
	"time"
)

// WindowPrompt renders one public history item for a private conversation.
// The bool distinguishes an empty line from an item absent from the prompt.
type WindowPrompt func(Item) (string, bool)

const WindowPromptSeparator = "\n\n"
const EmptyWindowPrompt = "Unreal Agent resumed this turn after an interruption. Continue the task."

func (p WindowPrompt) Render(items []Item) string {
	var lines []string
	for _, item := range items {
		if line, ok := p(item); ok {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return EmptyWindowPrompt
	}
	return strings.Join(lines, WindowPromptSeparator)
}

// ConversationBinding identifies a private conversation selected for an Unreal
// Session. The boundary is empty for the initial window.
type ConversationBinding struct {
	Provider string
	ID       string
	Boundary string `json:",omitzero"`
}

// ConversationRecord describes a provider's private conversation. Unknown
// post-compaction sizes stay unknown until the provider reports usage again.
type ConversationRecord struct {
	Provider            string
	InputTokens         int64
	InputKnown          bool
	BeforeTokens        int64     `json:",omitzero"`
	BeforeSource        string    `json:",omitzero"`
	AfterTokens         int64     `json:",omitzero"` // summary size; InputTokens is the actual model input
	AfterKnown          bool      `json:",omitzero"`
	SummaryOutputTokens *int64    `json:",omitzero"`
	DurationSource      string    `json:",omitzero"`
	Model               string    `json:",omitzero"`
	Compacted           bool      `json:",omitzero"`
	Interrupted         bool      `json:",omitzero"`
	Error               string    `json:",omitzero"`
	Phase               string    `json:",omitzero"`
	StartedAt           time.Time `json:",omitzero"`
	EndedAt             time.Time `json:",omitzero"`
	InterruptReason     string    `json:",omitzero"`
	Usage               *Usage    `json:",omitzero"`
}
