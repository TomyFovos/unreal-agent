package coordinator

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestProjectedRefreshContextBuiltPreservesPercentageDiagnostics(t *testing.T) {
	b := contextbuilder.NewBuilder()
	if _, err := contextbuilder.EnableCompaction(b, contextengine.Config{Version: 1, InputBudget: 24576}, contextengine.Constraints{}); err != nil {
		t.Fatal(err)
	}
	projection := llm.ContextProjection{Measure: func(llm.Request) (int64, error) { return 10557, nil }}
	expected, err := b.(interface {
		BuildWithProjection(llm.ContextProjection) (contextbuilder.Result, error)
	}).BuildWithProjection(projection)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(emptyFakeStore(), newTestInbox(t), newFakeOperationManager(), b, tool.NewRegistry(tool.StaticTranslators{}))
	c.modelOrigin = "current-turn"
	c.cancelModel = func() {}
	var observed *contextengine.Diagnostics
	c.dependencies.ContextBuilt = func(d contextengine.Diagnostics) { observed = &d }
	reply := make(chan bridgeReply, 1)
	if err := c.acceptBridgeRequest(t.Context(), bridgeRequest{ctx: t.Context(), origin: "current-turn", refresh: true, projection: &projection, reply: reply}); err != nil {
		t.Fatal(err)
	}
	response := <-reply
	if response.err != nil || response.budget != 24576 || observed == nil || observed.Utilization != 100*10557/float64(24576) || observed.Utilization != expected.Report.Context.Utilization || observed.EstimatedInputTokens != expected.Report.Context.EstimatedInputTokens {
		t.Fatal("Report.Context -> ContextBuilt changed units", response.err, observed)
	}
}
