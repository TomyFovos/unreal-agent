package contextbuilder

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func typedTestResult(t *testing.T, items []llm.Item, id string) llm.ToolResult {
	t.Helper()
	for _, item := range items {
		if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == id {
			return result
		}
	}
	t.Fatal("typed result missing from model projection")
	return llm.ToolResult{}
}

func TestTypedToolResultSurvivesCommitAndBuild(t *testing.T) {
	for _, state := range []llm.ToolResultState{llm.ToolResultUnknown, llm.ToolResultSucceeded, llm.ToolResultFailed} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			builder := NewBuilder().(*builder)
			builder.AddTypedToolResult(llm.ToolResult{CallID: "synthetic-call", State: state, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "synthetic receipt"}}}, false)
			builder.Commit()
			result, err := builder.Build()
			if err != nil {
				t.Fatal("cannot build synthetic typed result")
			}
			got := typedTestResult(t, result.Request.Input, "synthetic-call")
			if got.State != state || got.Output[0].Value != "synthetic receipt" {
				t.Fatal("commit lost Host-derived state or receipt body")
			}
		})
	}
}

func TestTypedPendingToolResultReplacedByTerminal(t *testing.T) {
	b := NewBuilder().(*builder)
	b.AddTypedToolResult(llm.ToolResult{CallID: "synthetic-call", State: llm.ToolResultPending}, true)
	request, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if result := typedTestResult(t, request.Request.Input, "synthetic-call"); result.State != llm.ToolResultPending || result.Output[0].Value != ToolCallRunningPayload {
		t.Fatal("pending state lost or invented terminal payload")
	}
	b.AddTypedToolResult(llm.ToolResult{CallID: "synthetic-call", State: llm.ToolResultFailed, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "synthetic failed receipt"}}}, false)
	request, err = b.Build()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range request.Request.Input {
		if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == "synthetic-call" {
			count++
			if result.State != llm.ToolResultFailed || result.Output[0].Value != "synthetic failed receipt" {
				t.Fatal("pending placeholder survived terminal replacement")
			}
		}
	}
	if count != 1 {
		t.Fatal("terminal update duplicated tool result")
	}
}

func TestTypedToolResultStateCannotBeJSONAuthority(t *testing.T) {
	result := llm.ToolResult{CallID: "synthetic-call", State: llm.ToolResultSucceeded}
	encoded, err := json.Marshal(result)
	if err != nil || bytes.Contains(encoded, []byte("State")) || bytes.Contains(encoded, []byte("state")) {
		t.Fatal("derived ToolResult state became a canonical JSON field")
	}
	for _, field := range []string{`"State":2`, `"state":2`, `"State":"succeeded"`} {
		var decoded llm.ToolResult
		if err := json.Unmarshal([]byte(`{"CallID":"synthetic-call","Output":[],`+field+`}`), &decoded); err != nil {
			t.Fatal("unknown JSON metadata policy changed")
		}
		if decoded.State != llm.ToolResultUnknown {
			t.Fatal("untrusted JSON forged Host-derived result success")
		}
	}
	var decoded llm.ToolResult
	if json.Unmarshal(encoded, &decoded) != nil || decoded.State != llm.ToolResultUnknown {
		t.Fatal("JSON round trip did not require Host reconstruction")
	}
}
