package main

import (
	"context"
	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"io"
)

func parseRequest(input io.Reader) (agentrunner.Request, agentrunner.ToolFactory, error) {
	var parsed agentrunner.Request
	if err := agentrunner.DecodeRequest(input, &parsed); err != nil {
		return agentrunner.Request{}, nil, err
	}
	return parsed, func(ctx context.Context, c agentrunner.ToolConfig) (agentrunner.Tools, error) {
		return agentrunner.DefaultTools(ctx, c, parsed.DisallowedTools)
	}, nil
}
