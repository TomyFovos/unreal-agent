package coordinator

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// The real subagent translator is tested in its own package; importing it here
// would form a cycle through Host. This probe observes Coordinator metadata.
type finishContextProbe struct {
	eligible bool
	waiting  []string
}

func (p *finishContextProbe) Translate(ctx tool.Context, _ llm.ToolCall) tool.CallStatus {
	c := ctx.(interface {
		CanFinish() bool
		FinishWaitingFor() []string
	})
	p.eligible, p.waiting = c.CanFinish(), c.FinishWaitingFor()
	return tool.CallStatus{}
}
func (*finishContextProbe) TranslateResult(id string, _ tool.CallStatus, _ []operation.Operation) (llm.ToolResult, error) {
	return llm.ToolResult{CallID: id}, nil
}

func TestFinishDiagnosticsNameOnlyUnfinishedCallsAndOperations(t *testing.T) {
	f := newToolAdmissionFixture(t)
	probe := &finishContextProbe{}
	var err error
	f.current.dependencies.Tools, err = tool.WithExtensions(f.registry, []tool.Extension{{Definition: tool.Definition{Tool: llm.Tool{Name: "Finish", Type: llm.ToolFunction}}, Translator: probe, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	key := toolCallKey{turnID: f.current.state.currentTurnID, callID: "finish"}
	finish := llm.ToolCall{CallID: "finish", Name: "Finish", Arguments: `{"status":"completed","summary":"done","changedFiles":[],"tests":[],"blockers":[]}`}
	f.current.state.toolCalls[key] = toolCallState{toolCall: finish}
	other := toolCallKey{turnID: key.turnID, callID: "pending-read"}
	f.current.state.toolCalls[other] = toolCallState{toolCall: llm.ToolCall{Name: "read", CallID: other.callID}}
	f.current.state.operations["pending-op"] = operation.Operation{ID: "pending-op", ToolName: "Bash", Status: operation.StatusAwaiting}
	f.current.state.operations["terminal-op"] = operation.Operation{ID: "terminal-op", ToolName: "Bash", Status: operation.StatusCompleted}
	status, err := f.current.scheduleToolCall(t.Context(), key, finish)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"call pending-read (read)", "operation pending-op (Bash, awaiting)"} {
		if !strings.Contains(strings.Join(probe.waiting, ", "), want) {
			t.Fatal(probe.waiting)
		}
	}
	if strings.Contains(strings.Join(probe.waiting, ", "), "terminal-op") || probe.eligible || len(status.Operations) != 0 {
		t.Fatal(status)
	}
	delete(f.current.state.toolCalls, other)
	op := f.current.state.operations["pending-op"]
	op.Status = operation.StatusCompleted
	f.current.state.operations[op.ID] = op
	f.current.state.toolCalls[key] = toolCallState{toolCall: finish}
	status, err = f.current.scheduleToolCall(t.Context(), key, finish)
	if err != nil || status.Status.Error != "" || !probe.eligible || len(probe.waiting) != 0 {
		t.Fatal("eligible Finish failed", status, err)
	}
}
