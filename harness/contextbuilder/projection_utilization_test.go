package contextbuilder

import (
	"fmt"
	"math"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestProjectionUtilizationIsPercentOfResolvedInputBudget(t *testing.T) {
	for _, estimate := range []int64{0, 1, 10557, 24576} {
		t.Run(fmt.Sprint(estimate), func(t *testing.T) {
			f := newContextFixture(t, "claude-code", contextengine.Config{Version: 1, InputBudget: 24576})
			if estimate < 24576 {
				f.user(t, "task", "synthetic task")
			}
			calls := 0
			projection := llm.ContextProjection{Measure: func(llm.Request) (int64, error) { calls++; return estimate, nil }}
			r, err := f.b.BuildWithProjection(projection)
			if err != nil {
				t.Fatal(err)
			}
			d := r.Report.Context
			want := 100 * float64(estimate) / 24576
			if d == nil || d.Budget.Input != 24576 || d.EstimatedInputTokens != estimate || math.Abs(d.Utilization-want) > 1e-10 {
				t.Fatalf("diagnostics=%+v, want %.10f%%", d, want)
			}
			final, err := projection.Measure(r.Request)
			if err != nil || final != d.EstimatedInputTokens || calls < 2 {
				t.Fatal("candidate/final measure diverged")
			}
		})
	}
	t.Run("zero configured budget resolves before division", func(t *testing.T) {
		f := newContextFixture(t, "claude-code", contextengine.Config{Version: 1, InputBudget: 0})
		r, err := f.b.BuildWithProjection(llm.ContextProjection{Measure: func(llm.Request) (int64, error) { return 100, nil }})
		if err != nil {
			t.Fatal(err)
		}
		d := r.Report.Context
		if d.Budget.Input <= 0 || math.IsNaN(d.Utilization) || math.IsInf(d.Utilization, 0) || d.Utilization != 100*100/float64(d.Budget.Input) {
			t.Fatal("zero config did not use resolved denominator", d)
		}
	})
}
