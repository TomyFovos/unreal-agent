package host

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativeTool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

func TestNativeBatchReadKeepsL3ReceiptWhenExcerpted(t *testing.T) {
	ctx, workspace := t.Context(), t.TempDir()
	const needle = "middle-only-read-evidence"
	middle := strings.Repeat("padding ", 80) + needle + strings.Repeat(" padding", 70) + "\n"
	checkHostRestore(t, os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("first line\n"+middle+"last line\n"), 0600))
	checkHostRestore(t, os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("other file\n"), 0600))
	received := make(chan llm.ToolResult, 1)
	h, err := New(ctx, Config{Directory: t.TempDir(), Build: func(ctx context.Context, id session.ID) (Runtime, error) {
		files, err := mutation.New(mutation.Config{Root: workspace, StateDir: t.TempDir(), Authorize: func(ctx context.Context, path string, write bool) error {
			return permission.FromContext(ctx).CheckPath(path, write)
		}})
		if err != nil {
			return Runtime{}, err
		}
		handler, err := native.NewHandler(ctx, native.Executor{Files: files}, string(id))
		if err != nil {
			return Runtime{}, err
		}
		reg := tool.NewRegistry(nativeTool.Configure(tool.StaticTranslators{}), tool.ReadName)
		builder, engine, err := windowBuilder(reg, contextengine.Config{ToolOutputExcerptChars: 40})
		if err != nil {
			handler.Close()
			return Runtime{}, err
		}
		model := modelFunc(func(ctx context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
			for _, item := range request.Input {
				if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == "batch" {
					received <- result
					return llm.Response{Stop: llm.StopComplete}, nil
				}
			}
			call := llm.ToolCall{CallID: "batch", Name: tool.ReadName, Arguments: `{"files":[{"path":"a.txt","ranges":[{"start_line":2,"lines":1}]},{"path":"b.txt"}]}`}
			return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}, nil
		})
		return Runtime{Builder: builder, ContextEngine: engine, Tools: reg, Operations: operation.NewLocalOperationManager(ctx, handler), LLM: model, Close: handler.Close}, nil
	}})
	checkHostRestore(t, err)
	defer h.Close()
	s, err := h.Open(ctx, Options{ID: "batch-read", Workspace: workspace, Policy: permission.Unrestricted(), Initial: []inbox.Input{input("task", "Read both files.")}})
	checkHostRestore(t, err)
	var result llm.ToolResult
	select {
	case result = <-received:
	case <-timeout(t).Done():
		t.Fatal("native Read did not return a projected receipt")
	}
	if len(result.Output) != 2 || strings.Contains(result.Output[0].Value, needle) {
		t.Fatalf("Read result was not excerpted: %+v", result.Output)
	}
	matches, err := s.RecordFind(ctx, record.Query{Kind: record.OperationOutput, Text: needle})
	checkHostRestore(t, err)
	if len(matches) != 1 || !strings.Contains(result.Output[1].Value, string(matches[0].Locator)) {
		t.Fatalf("missing L3 locator for the batch: %+v, %+v", result.Output, matches)
	}
	page, err := s.RecordShow(ctx, matches[0].Locator, 0)
	checkHostRestore(t, err)
	var full struct{ Handle native.Result }
	checkHostRestore(t, json.Unmarshal(page.Bytes, &full))
	if full.Handle.Locator != matches[0].Locator || len(full.Handle.Files) != 2 || len(full.Handle.Files[0].Ranges) != 1 ||
		full.Handle.Files[0].Ranges[0].Content != middle || len(full.Handle.Files[1].Ranges) != 1 ||
		full.Handle.Files[1].Ranges[0].Content != "other file\n" {
		t.Fatalf("L3 did not preserve the batch result: %+v", full.Handle)
	}
}
