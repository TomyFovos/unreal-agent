package coordinator

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func (current *coordinator) projectToolResult(status sessionstore.ToolCallStatus, result llm.ToolResult, translator tool.ResultTranslator, running bool) error {
	// Call identity belongs to the canonical status, as in AddToolResult's
	// original contract, not an optional field populated by a Translator.
	result.CallID = status.CallID
	if !llm.RequiresToolBatch(current.dependencies.LLM) {
		current.dependencies.ContextBuilder.AddToolResult(result.CallID, result.Output, running)
		return nil
	}
	builder, ok := current.dependencies.ContextBuilder.(interface {
		AddTypedToolResult(llm.ToolResult, bool)
	})
	if !ok {
		return llm.ErrToolRecoveryRequired
	}
	result.State = terminalResultState(status, translator, running)
	if current.nativeConversation() || current.conversation != nil {
		current.observeTypedToolResult(status, translator, &result, running)
	}
	builder.AddTypedToolResult(result, running)
	return nil
}

func terminalResultState(status sessionstore.ToolCallStatus, translator tool.ResultTranslator, running bool) llm.ToolResultState {
	if running {
		return llm.ToolResultPending
	}
	failed := status.Status.Error != "" || status.Status.Denial != nil
	if classifier, ok := translator.(tool.ResultFailureClassifier); ok {
		failed = failed || classifier.ResultFailed(status.Status, status.Operations)
	}
	for _, op := range status.Operations {
		if !operationIsTerminal(op.Status) {
			return llm.ToolResultPending
		}
		failed = failed || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled || op.Denial != nil
		if op.Type == operation.TypeShell && op.Status == operation.StatusCompleted {
			state, err := operation.DecodeShellState(op)
			if err != nil || state.Result == nil {
				return llm.ToolResultUnknown
			}
			failed = failed || state.Result.ExitCode != 0
		}
	}
	if failed {
		return llm.ToolResultFailed
	}
	if translator == nil {
		return llm.ToolResultUnknown
	}
	return llm.ToolResultSucceeded
}

// A successful explicit Host classifier is receipt evidence, not the default
// false value of a failure bool. A tool type or terminal ExitCode alone does
// not prove that the surrounding execution state is valid.
func (current *coordinator) observeTypedToolResult(status sessionstore.ToolCallStatus, translator tool.ResultTranslator, result *llm.ToolResult, running bool) {
	if running {
		return
	}
	_, classified := translator.(tool.ResultFailureClassifier)
	proven := result.State == llm.ToolResultSucceeded && classified
	for _, op := range status.Operations {
		if current.conversation != nil {
			delete(current.conversation.pending, op.ID)
		}
		if proven {
			if current.conversation != nil {
				delete(current.conversation.quarantined, op.ID)
				delete(current.conversation.effects, op.ID)
			}
			continue
		}
		current.observeUncertainty(op, false)
		if current.quarantined(op.ID) {
			result.State = llm.ToolResultUnknown
		}
	}
}
