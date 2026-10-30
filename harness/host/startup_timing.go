package host

import (
	"context"
	"log/slog"
	"runtime/trace"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// Phase timings contain no history or provider payloads. They are available in
// Host logs and runtime traces, including slow restorations seen in production.
func startupPhase(ctx context.Context, id session.ID, phase string) func() {
	start := time.Now()
	region := trace.StartRegion(ctx, phase)
	return func() {
		region.End()
		slog.InfoContext(ctx, "session startup", "session", id, "phase", phase, "elapsed", time.Since(start))
	}
}
