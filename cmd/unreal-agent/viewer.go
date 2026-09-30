package main

import (
	"context"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func withViewer(config gateway.Config, directory string) gateway.Config {
	config.Extension = viewer.ExtensionHandler{Host: config.Host, Directory: directory}
	return config
}
func attachViewer(ctx context.Context, client *gateway.Client, id session.ID) error {
	remote := viewer.Remote{ParentID: id, Parent: client, Call: client.Extension}
	observation := viewer.NewClient(remote, viewer.New(viewer.SubagentOptions()), remote)
	panel := viewer.NewPanel(ctx, id, observation)
	defer panel.Close()
	return tui.Terminal(ctx, tui.Config{Client: client, ID: id, Panel: panel.Render, Command: panel.Command, Updates: panel.Updates()})
}
