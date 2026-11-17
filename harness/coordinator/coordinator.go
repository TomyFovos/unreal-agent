// Package coordinator defines the owner of the central event loop.
package coordinator

import (
	"context"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type Dependencies struct {
	RecordActivity func(context.Context, llm.ActivityRecord) error
	// ModelWaiting publishes the usage-limit wait; zero clears it.
	ModelWaiting func(context.Context, time.Time) error
	// Child Sessions keep their execution ownership after a transient inference
	// failure. Other lifecycles retain their configured provider retry policy.
	RetryModelFailures bool
	// InterruptedInput supplies a peer notice for work lost with the previous
	// executor. Restoration persists it before closing the Operation.
	InterruptedInput func(operation.Operation) (*inbox.Input, error)
	RetainOperation  func(context.Context, operation.Operation) (bool, error)
	// CompletedInput projects a standalone child's terminal Operation after the
	// current model call has ended, so a report it already read can be omitted.
	CompletedInput func(operation.Operation) (*inbox.Input, error)
	// ContextBuilt publishes derived, body-free request diagnostics. It does
	// not append a canonical record or change scheduling/lifecycle decisions.
	ContextBuilt func(contextengine.Diagnostics)
	// BeforeTurn is owner composition, called only on the coordinator goroutine.
	// boundary excludes an in-flight request and unfinished tool/operation loops.
	BeforeTurn func(context.Context, bool) (uint64, error)
	// BeforeGeneration refreshes only ephemeral owner-controlled guidance on the
	// Coordinator goroutine. It must not apply a pending runtime selection.
	BeforeGeneration func(context.Context) error
	// InitialInputs is one startup batch processed before any execution or model decision.
	InitialInputs []inbox.Input
	// Finished is a pure read of an already committed canonical Finish record.
	Finished func() bool
	// AfterResponse can commit an Unreal-owned text-only child's completion
	// after its response is durable. It never invents a model tool call.
	AfterResponse func(context.Context, session.TurnID) error
	// OnIdle commits child completion only after all model/tool work and waking
	// inputs have drained. The owner preserves an explicit parent-reply wait.
	OnIdle func(context.Context, session.TurnID) error
	// RestoredStop is a persisted stop whose effect was not completed.
	RestoredStop          *inbox.ControlMessage
	ToolHeartbeatInterval time.Duration
	SessionID             session.ID
	// CacheKey groups requests with a shared prompt prefix. Empty retains
	// the Session ID, independently of the canonical request identity.
	CacheKey       string
	Inbox          *inbox.Inbox
	Restored       sessionstore.ResumeState
	Sessions       sessionstore.Store
	ContextBuilder contextbuilder.Builder
	LLM            llm.Adapter
	Tools          tool.Registry
	Operations     operation.Manager
}

type Coordinator interface {
	// Run owns one session's decision loop until a stop control completes or
	// the context is canceled. It returns nil for a completed stop. It is
	// single-use; its caller must cancel the Inbox when Run returns.
	Run(context.Context) error
}

func New(dependencies Dependencies) Coordinator {
	return &coordinator{
		dependencies: dependencies,
		state:        newLoopState(),
	}
}
