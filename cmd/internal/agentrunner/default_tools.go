package agentrunner

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/ast"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	astTool "github.com/unreallabsai/unreal-agent/harness/tool/ast"
	nativeTool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

// DefaultTools composes handlers once per session. The caller owns Close and must
// close all handlers, which cancel and drain their own workers.
func DefaultTools(ctx context.Context, config ToolConfig, disallowed []string) (Tools, error) {
	files, err := mutation.New(mutation.Config{Root: config.Directory, Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		return Tools{}, err
	}
	handler, err := native.NewHandler(ctx, native.Executor{Files: files}, string(config.SessionID))
	if err != nil {
		return Tools{}, err
	}
	structural, err := ast.NewHandler(ctx, ast.Executor{Files: files}, string(config.SessionID))
	if err != nil {
		handler.Close()
		return Tools{}, err
	}
	names := append(append([]string(nil), config.Names...), tool.ReadName, tool.WriteName, tool.EditName, tool.GrepName, tool.GlobName, tool.ASTGrepName, tool.ASTEditName)
	enabled := (Request{DisallowedTools: disallowed}).EnabledTools(names...)
	return Tools{Registry: tool.NewRegistry(astTool.Configure(nativeTool.Configure(config.Translators)), enabled...), RemoteJobs: []operation.RemoteJobHandler{handler, structural}, Close: func() error { return errors.Join(handler.Close(), structural.Close()) }}, nil
}
