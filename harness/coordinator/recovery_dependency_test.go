package coordinator

import (
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"testing"
)

func TestRecoveryExecutionDependenciesFailClosed(t *testing.T) {
	for _, mode := range []string{"uncertain", "missing", "nonterminal", "cycle", "known-complete", "result-uncertain"} {
		t.Run(mode, func(t *testing.T) {
			c := &coordinator{state: newLoopState()}
			a := quarantineTestNative("write", "a")
			a.ID = "uncertain-a"
			c.state.operations[a.ID] = a
			c.markUncertain(a.ID)
			candidate := quarantineTestNative("write", "b")
			candidate.ID = "new-b"
			candidate = operation.WithDependencies(candidate, []operation.ID{a.ID})
			switch mode {
			case "missing":
				candidate = operation.WithDependencies(candidate, []operation.ID{"missing"})
			case "nonterminal":
				c.conversation.quarantined = map[operation.ID]bool{}
			case "cycle", "known-complete":
				one := operation.Operation{ID: "one", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusCompleted, State: quarantineTestJSON(operation.ValueState{Value: []byte(`true`)})}
				if mode == "cycle" {
					one = operation.WithDependencies(one, []operation.ID{"one"})
				}
				c.state.operations[one.ID] = one
				candidate = operation.WithDependencies(candidate, []operation.ID{one.ID})
			case "result-uncertain":
				a.Status = operation.StatusFailed
				c.state.operations[a.ID] = a
				c.conversation.quarantined = map[operation.ID]bool{}
			}
			if c.dependenciesCertain(candidate) != (mode == "known-complete") {
				t.Fatal("uncertain/result/source/order dependency was treated as independent")
			}
		})
	}
}
func TestHeldUnexecutedOperationDoesNotCreateNewUncertainty(t *testing.T) {
	c := &coordinator{state: newLoopState(), dependencies: Dependencies{LLM: resultRecoveryAdapter{}}}
	a := quarantineTestNative("write", "a")
	a.ID = "uncertain-a"
	c.state.operations[a.ID] = a
	c.markUncertain(a.ID)
	b := quarantineTestNative("write", "b")
	b.ID = "held-b"
	held := operation.PermissionFailure(b, recoveryDenial("unproven"))
	held.Permission = &permission.Decision{State: "denied"}
	if operationUncertain(held) {
		t.Fatal("definite pre-execution hold invented an unknown effect")
	}
	c.observeUncertainty(held, false)
	if c.quarantined(b.ID) || !c.quarantined(a.ID) {
		t.Fatal("holding a new proposal erased A uncertainty or created another effect")
	}
}
