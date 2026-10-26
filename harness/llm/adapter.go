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
	// Tools is a host-owned rendezvous for protocols that keep inference open
	// while caller-owned tools execute. The host commits this public response,
	// runs its Registry/Permission/Operation path and returns terminal receipts.
	// The adapter never executes work or treats progress as a receipt.
	Tools func(context.Context, Response) ([]ToolOutcome, error)
	// BeginTools pins a live host-rendezvous request before its first tool call.
	// Queued inputs must wait for the final response rather than restart this
	// process while its private continuation is active.
	BeginTools func(context.Context) error
	// RefreshContext rebuilds a bounded projection after canonical receipts have
	// been committed. It never mutates the runtime or exposes provider-private
	// continuation state. Multi-generation adapters require this host callback.
	RefreshContext func(context.Context, int64) (Request, int64, error)
	// RefreshProjectedContext selects with the pinned adapter's pure final-wire
	// measure. Only the coordinator invokes it on the owned Builder; the measure
	// performs no execution, I/O, authentication, runtime switch or persistence.
	RefreshProjectedContext func(context.Context, ContextProjection) (Request, int64, error)
	// ActionScope identifies the canonical input/task for request-local action
	// IDs. Adapters must not choose a new scope for a continuation after resume.
	ActionScope string
	// InputBudget is the Context Engine's input ceiling for this runtime. A
	// streaming caller-tool adapter must bound its request-lifetime continuation.
	InputBudget int64
}

type RequestIdentity struct {
	SessionID       string
	TurnID          string
	RuntimeRevision uint64
}

// ContextProjection is request-local trusted code, never provider output or a
// canonical field. Reserve is the measured empty transport/framing cost;
// Measure includes that same cost, system, escaping and selected receipt data.
type ContextProjection struct {
	Reserve       int64
	SchemaReserve int64
	Measure       func(Request) (int64, error)
}

type ToolOutcome struct {
	Result ToolResult
	Failed bool
}

type Adapter interface {
	Respond(context.Context, Request, RequestOptions) (Response, error)
}

// RequiresToolBatch is an opt-in transport requirement, not execution authority.
// Legacy providers retain their existing asynchronous continuation semantics.
func RequiresToolBatch(adapter Adapter) bool {
	capability, ok := adapter.(interface{ RequiresFullToolBatch() bool })
	return ok && capability.RequiresFullToolBatch()
}

// RequiresExplicitReplay opts into a canonical pre-send marker. An unanswered
// request can have reached the server; restore alone cannot authorize a repeat.
func RequiresExplicitReplay(adapter Adapter) bool {
	capability, ok := adapter.(interface{ NoAutomaticReplay() bool })
	return ok && capability.NoAutomaticReplay()
}
