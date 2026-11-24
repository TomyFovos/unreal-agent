package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"uuid"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// The child's Host owns its starts and approval answers. The root Host only
// owns the lead, so Bash needs a socket in the lead's own process.
func configureClaudeChildCommands(runtime *agentrunner.RuntimeConfig, c subagent.ChildConfig, modelFile string) error {
	name := "unreal-child-" + uuid.New().String()[:12]
	directory := filepath.Join(os.TempDir(), name)
	socket := filepath.Join(directory, "host.sock")
	if !filepath.IsAbs(directory) || len(socket) > 100 {
		directory = filepath.Join("/tmp", name)
		socket = filepath.Join(directory, "host.sock")
	}
	cli, err := claudeSubagents(c.Subagents, socket, "")
	if err != nil {
		return err
	}
	cli.ChildCommand, cli.ContextCommand = cli.ContextCommand, ""
	if len(c.Subagents) > 0 && modelFile != "" {
		scope := subagentModelScope{defaultFile: modelFile, sessionDirectory: c.SessionDirectory, parent: c.ParentID}
		cli.Roles = subagentRoles(*runtime, scope, c.Subagents)
	}
	runtime.ClaudeSubagents = cli
	layer := c.Layer
	if layer == "" {
		layer = subagent.RoleGroup(c.Template)
	}
	configuration := *runtime
	runtime.ServeChildCommands = func(ctx context.Context, owner *host.Host) (io.Closer, error) {
		if err := os.Mkdir(directory, 0700); err != nil {
			return nil, err
		}
		config := withViewer(gateway.Config{Host: owner}, c.SessionDirectory)
		config.Extension = modelUI{owner: owner, runtime: configuration, templates: c.Subagents, childLayer: layer, next: config.Extension}
		commands, err := gateway.StartSessionCommands(ctx, socket, config)
		if err != nil {
			_ = os.RemoveAll(directory)
			return nil, err
		}
		return closeFunc(func() error { return errors.Join(commands.Close(), os.RemoveAll(directory)) }), nil
	}
	return nil
}
