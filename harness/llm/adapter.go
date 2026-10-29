package llm

import (
	"context"
	"errors"
)

var ErrToolIdentityConflict = errors.New("conflicting canonical tool request identity")
var ErrToolRecoveryRequired = errors.New("canonical tool request requires operation recovery")

type RequestOptions struct {
	CacheKey string
	// Identity correlates this request with its already persisted canonical turn.
	// It is private request metadata and is never serialized to the provider.
	Identity RequestIdentity
	// Progress is optional, ephemeral and never canonical history. Callbacks must be fast.
	Progress func(Progress)
	// ConversationTokens is the last provider-reported context size, restored
	// by the Host. It describes the provider's private conversation, not the
	// canonical input projection.
	ConversationTokens map[string]int64
	// RecordConversation appends a small observation without replacing history.
	RecordConversation func(context.Context, ConversationRecord) error
	// Tools is a host-owned rendezvous for protocols that keep inference open
	// while caller-owned tools execute. The host commits this public response,
	// runs its Registry/Permission/Operation path and returns terminal receipts.
	// The adapter never executes work or treats progress as a receipt.
	Tools func(context.Context, Response) ([]ToolOutcome, error)
	// BeginTools pins a live host-rendezvous request before its first tool call.
	// Queued inputs wait for the final response while its private continuation
	// is active, except when waitingForChildren lets user input interrupt it
	// or the provider delivers them through Steering at a batch boundary.
	// The adapter updates that flag at the beginning and end of such a wait.
	BeginTools func(ctx context.Context, waitingForChildren bool) error
	// Steering delivers queued user input at a provider's tool-batch boundary.
	// The Coordinator records delivery in the live turn's input watermark.
	// Only providers that consume it opt in; other routes are unchanged.
	Steering func(context.Context) ([]string, error)
}

type RequestIdentity struct {
	SessionID       string
	TurnID          string
	RuntimeRevision uint64
}

type ToolOutcome struct {
	Result ToolResult
	Failed bool
}

type Adapter interface {
	Respond(context.Context, Request, RequestOptions) (Response, error)
}

// IdleAdapter performs optional provider maintenance after Respond has returned
// and the Coordinator has no model call or pending tool/operation. The owner
// cancels and joins Idle before starting new work or changing the runtime.
type IdleAdapter interface {
	Idle(context.Context, Request, RequestOptions)
}

// RequiresToolBatch is an opt-in transport requirement, not execution authority.
// Legacy providers retain their existing asynchronous continuation semantics.
func RequiresToolBatch(adapter Adapter) bool {
	capability, ok := adapter.(interface{ RequiresFullToolBatch() bool })
	return ok && capability.RequiresFullToolBatch()
}

// IsContextOverflow recognizes the provider's structured context-window error.
func IsContextOverflow(err error, response Response) bool {
	var coded interface{ ContextOverflow() bool }
	if errors.As(err, &coded) && coded.ContextOverflow() {
		return true
	}
	return response.Failure != nil && (response.Failure.Code == "context_length_exceeded" || response.Failure.Code == "context_window_exceeded")
}
