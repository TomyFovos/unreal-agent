package observed

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

type submitted struct{ specs []operation.Spec }

func (s *submitted) Submit(spec operation.Spec) operation.ID {
	s.specs = append(s.specs, spec)
	return operation.ID("op-1")
}

func completed(spec operation.Spec) operation.Operation {
	return operation.Operation{ID: "op-1", Type: spec.Type, Version: spec.Version, Status: operation.StatusCompleted, State: spec.State}
}

func TestReportedResultBecomesValueReceipt(t *testing.T) {
	results := NewResults()
	translator := New(results)
	results.Put("toolu_1", Result{Content: "The file sample.txt has been updated."})
	ctx := &submitted{}
	status := translator.Translate(ctx, llm.ToolCall{CallID: "toolu_1", Name: "claude_code", Arguments: `{"tool":"Edit","input":{"file_path":"/w/sample.txt"}}`})
	if status.Error != "" || len(ctx.specs) != 1 || ctx.specs[0].Type != operation.TypeValue {
		t.Fatalf("status=%+v specs=%+v", status, ctx.specs)
	}
	op := completed(ctx.specs[0])
	got, err := translator.TranslateResult("toolu_1", status, []operation.Operation{op})
	if err != nil || len(got.Output) != 1 || got.Output[0].Value != "The file sample.txt has been updated." {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if translator.ResultFailed(status, []operation.Operation{op}) {
		t.Fatal("a successful call was classified as failed")
	}
	// The result is handed over once; a replayed call cannot invent one.
	if again := translator.Translate(&submitted{}, llm.ToolCall{CallID: "toolu_1", Name: "claude_code", Arguments: `{"tool":"Edit"}`}); again.Error == "" {
		t.Fatal("a second Translate reused the reported result")
	}
}

// A denied or failed call is a failed receipt with the reason Claude read, so
// it never shows as a completed record.
func TestReportedErrorIsAFailedReceipt(t *testing.T) {
	results := NewResults()
	translator := New(results)
	results.Put("toolu_2", Result{Content: "PreToolUse:Bash hook error: Unreal Agent is in Read Only mode, so Bash calls are denied.", IsError: true})
	ctx := &submitted{}
	status := translator.Translate(ctx, llm.ToolCall{CallID: "toolu_2", Name: "claude_code", Arguments: `{"tool":"Bash","input":{"command":"curl --version"}}`})
	if len(ctx.specs) != 0 || !strings.Contains(status.Error, "Read Only mode") {
		t.Fatalf("status=%+v specs=%d", status, len(ctx.specs))
	}
	if !translator.ResultFailed(status, nil) {
		t.Fatal("an error result was classified as successful")
	}
	got, err := translator.TranslateResult("toolu_2", status, nil)
	if err != nil || got.Output[0].Value != status.Error {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestLongResultKeepsHeadAndTail(t *testing.T) {
	results := NewResults()
	translator := New(results)
	results.Put("toolu_3", Result{Content: "HEAD" + strings.Repeat("x", 2*operation.DefaultMaxOutputLength) + "TAIL"})
	ctx := &submitted{}
	status := translator.Translate(ctx, llm.ToolCall{CallID: "toolu_3", Name: "claude_code", Arguments: `{"tool":"Read"}`})
	got, err := translator.TranslateResult("toolu_3", status, []operation.Operation{completed(ctx.specs[0])})
	if err != nil {
		t.Fatal(err)
	}
	text := got.Output[0].Value
	if !strings.HasPrefix(text, "HEAD") || !strings.HasSuffix(text, "TAIL") || !strings.Contains(text, "bytes truncated") {
		t.Fatalf("bounded result lost its head, tail or marker: %d chars", len(text))
	}
}

func TestCallWithoutReportedResultIsAnError(t *testing.T) {
	translator := New(NewResults())
	status := translator.Translate(&submitted{}, llm.ToolCall{CallID: "toolu_4", Name: "claude_code", Arguments: `{"tool":"Read"}`})
	if status.Error == "" {
		t.Fatal("a call with no reported result was accepted")
	}
	got, err := translator.TranslateResult("toolu_4", status, nil)
	if err != nil || got.Output[0].Value != status.Error {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}
