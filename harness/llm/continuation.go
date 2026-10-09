package llm

// ContinuationError contains a fixed structural reason, never provider IDs,
// call arguments, response bodies or private reasoning.
type ContinuationError struct{ Code string }

func (e *ContinuationError) Error() string { return "model input: " + e.Code }

// ValidateContinuation runs on the final typed projection before network I/O.
// Async running/final result updates are legal; output existence/order is the
// invariant, not a one-to-one count. It never executes or authorizes a tool.
func ValidateContinuation(items []Item) error {
	calls := map[string]ToolCall{}
	outputs := map[string]bool{}
	for _, item := range items {
		switch item.Type {
		case ItemToolCall:
			call, ok := item.Data.(ToolCall)
			if !ok || call.CallID == "" {
				return &ContinuationError{"invalid_native_call"}
			}
			if prior, exists := calls[call.CallID]; exists && prior != call {
				return &ContinuationError{"conflicting_native_call"}
			}
			calls[call.CallID] = call
		case ItemToolResult:
			result, ok := item.Data.(ToolResult)
			if !ok || result.CallID == "" {
				return &ContinuationError{"invalid_native_output"}
			}
			if _, exists := calls[result.CallID]; !exists {
				return &ContinuationError{"native_output_without_call"}
			}
			outputs[result.CallID] = true
		}
	}
	for id := range calls {
		if !outputs[id] {
			return &ContinuationError{"native_call_without_output"}
		}
	}
	return nil
}
