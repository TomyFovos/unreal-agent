package llm

import (
	"context"
	"errors"
)

var ErrToolIdentityConflict = errors.New("conflicting canonical tool request identity")
var ErrToolRecoveryRequired = errors.New("canonical tool request requires operation recovery")

type RequestOptions struct {
	CacheKey string
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
	// ActionScope identifies the canonical input/task for request-local action
	// IDs. Adapters must not choose a new scope for a continuation after resume.
	ActionScope string
	// InputBudget is the Context Engine's input ceiling for this runtime. A
	// streaming caller-tool adapter must bound its request-lifetime continuation.
	InputBudget int64
}

type ToolOutcome struct {
	Result ToolResult
	Failed bool
}

type Adapter interface {
	Respond(context.Context, Request, RequestOptions) (Response, error)
}
