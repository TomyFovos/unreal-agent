package claudecode

import (
	"encoding/json/v2"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	files "github.com/unreallabsai/unreal-agent/harness/native"
)

// Preserve every budget-fit Builder output verbatim. An explicit ResultBytes
// cap may produce a typed partial native read, retaining its full-file revision,
// starting line and truncated=true. Never split a serialized JSON document or
// substitute a raw canonical result for a redacted/portable projection.
func boundedStructuredReceipt(result llm.ToolResult, name string, limit int) (llm.ToolResult, error) {
	if len(result.Output) > 128 {
		return llm.ToolResult{}, &Error{Code: "bridge_receipt_invalid"}
	}
	result.Output = slices.Clone(result.Output)
	remaining := limit
	for i, out := range result.Output {
		if out.Kind != llm.ToolResultText {
			return llm.ToolResult{}, &Error{Code: "bridge_receipt_invalid"}
		}
		if len(out.Value) > remaining {
			// Only a known native read can safely expose a smaller typed range.
			// Other oversized outputs fail closed; callers can request a bounded
			// tool result rather than receive mutilated JSON or invented success.
			var read files.Result
			if name != "read" || remaining <= 0 || json.Unmarshal([]byte(out.Value), &read, json.RejectUnknownMembers(true)) != nil || read.Version != files.Version || read.Code != "ok" || read.Revision == nil || read.Revision.Validate() != nil || read.Path == "" || read.Content == "" || read.Binary || read.Denial != nil || read.Mutation != nil || len(read.Matches) != 0 {
				return llm.ToolResult{}, &Error{Code: "bridge_context_budget"}
			}
			content := read.Content
			read.Content, read.Truncated = "", true
			if len(files.EncodeResult(read)) > remaining {
				return llm.ToolResult{}, &Error{Code: "bridge_context_budget"}
			}
			// JSON escaping contributes to the cap. Search byte-prefix lengths
			// using the native UTF-8 bound, then serialize the complete typed value.
			low, high := 0, len(content)
			for low < high {
				mid := low + (high-low+1)/2
				read.Content, _ = files.BoundText(content, mid)
				if len(files.EncodeResult(read)) <= remaining {
					low = mid
				} else {
					high = mid - 1
				}
			}
			read.Content, _ = files.BoundText(content, low)
			out.Value = string(files.EncodeResult(read))
		}
		remaining -= len(out.Value)
		result.Output[i] = out
	}
	return result, nil
}
