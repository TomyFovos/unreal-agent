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
	// ContextBuilt publishes derived, body-free request diagnostics. It does
	// not append a canonical record or change scheduling/lifecycle decisions.
	ContextBuilt func(contextengine.Diagnostics)
	// BeforeTurn is owner composition, called only on the coordinator goroutine.
	// boundary excludes an in-flight request and unfinished tool/operation loops.
	BeforeTurn func(context.Context, bool) (uint64, error)
	// InitialInputs is one startup batch processed before any execution or model decision.
	InitialInputs []inbox.Input
	// Finished is a pure read of an already committed canonical Finish record.
	Finished func() bool
	// AfterResponse can commit an Unreal-owned text-only child's completion
	// after its response is durable. It never invents a model tool call.
	AfterResponse func(context.Context, session.TurnID) error
	// RestoredStop is a persisted stop whose effect was not completed.
	RestoredStop          *inbox.ControlMessage
	ToolHeartbeatInterval time.Duration
	SessionID             session.ID
	Inbox                 *inbox.Inbox
	Restored              sessionstore.ResumeState
	Sessions              sessionstore.Store
	ContextBuilder        contextbuilder.Builder
	LLM                   llm.Adapter
	Tools                 tool.Registry
	Operations            operation.Manager
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
