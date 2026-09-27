package main

import (
	"context"
	"io"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	nativeTool "github.com/unreallabsai/unreal-agent/harness/tool/native"
)

func parseRequest(input io.Reader) (agentrunner.Request, agentrunner.ToolFactory, error) {
	var parsed agentrunner.Request
	if err := agentrunner.DecodeRequest(input, &parsed); err != nil {
		return agentrunner.Request{}, nil, err
	}
	return parsed, func(ctx context.Context, config agentrunner.ToolConfig) (agentrunner.Tools, error) {
		files, err := mutation.New(mutation.Config{Root: config.Directory, Authorize: func(ctx context.Context, path string, write bool) error {
			return permission.FromContext(ctx).CheckPath(path, write)
		}})
		if err != nil {
			return agentrunner.Tools{}, err
		}
		handler, err := native.NewHandler(ctx, native.Executor{Files: files}, string(config.SessionID))
		if err != nil {
			return agentrunner.Tools{}, err
		}
		return agentrunner.Tools{Registry: tool.NewRegistry(nativeTool.Configure(config.Translators), parsed.EnabledTools(append(append([]string(nil), config.Names...), tool.ReadName, tool.WriteName, tool.EditName, tool.GrepName, tool.GlobName)...)...), RemoteJobs: []operation.RemoteJobHandler{handler}}, nil
	}, nil
}
