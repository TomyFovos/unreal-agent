package main

import (
	"context"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"os"
	"path/filepath"
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
	return tui.Terminal(ctx, tui.Config{Client: client, ID: id, Viewer: panel.Snapshot, Command: childStartCommand(client, id, panel.Command), Updates: panel.Updates(),
		ViewChanged: func(mode tui.ViewMode) { panel.SetTopologyVisible(mode != tui.ViewChat) },
		RuntimeCapabilities: func(ctx context.Context) (map[string]modelcatalog.Capabilities, error) {
			out, err := modelExchange(ctx, client.Extension, modelRequest{Action: "runtime.health", ID: id})
			return out.Capabilities, err
		},
		ProviderCatalogs: func(ctx context.Context) ([]modelcatalog.Provider, error) {
			out, e := modelExchange(ctx, client.Extension, modelRequest{Action: "model.providers", ID: id})
			return out.Providers, e
		},
		ProviderCatalog: func(ctx context.Context, provider string, refresh bool) (modelcatalog.Catalog, error) {
			out, e := modelExchange(ctx, client.Extension, modelRequest{Action: "model.catalog", ID: id, Provider: provider, Refresh: refresh})
			if out.Catalog == nil {
				return modelcatalog.Catalog{}, e
			}
			return *out.Catalog, e
		},
		ModelCatalog: func(ctx context.Context) (modelcatalog.Catalog, error) {
			out, err := modelExchange(ctx, client.Extension, modelRequest{Action: "model.catalog", ID: id})
			if out.Catalog == nil {
				return modelcatalog.Catalog{Problem: "catalog unavailable"}, err
			}
			return *out.Catalog, err
		},
		RefreshModelCatalog: func(ctx context.Context) (modelcatalog.Catalog, error) {
			out, err := modelExchange(ctx, client.Extension, modelRequest{Action: "model.catalog", ID: id, Refresh: true})
			if out.Catalog == nil {
				return modelcatalog.Catalog{Problem: "catalog unavailable"}, err
			}
			return *out.Catalog, err
		},
		SelectModel: func(ctx context.Context, generation string, expected uint64, choice sessionstore.RuntimeSelection) error {
			_, err := modelExchange(ctx, client.Extension, modelRequest{Action: "model.select", ID: id, Generation: generation, Expected: expected, Selection: choice})
			return err
		},
		ExportAnalysis: func(ctx context.Context, report analysis.Report, format string) (string, error) {
			root := os.Getenv("XDG_STATE_HOME")
			if root == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return "", err
				}
				root = filepath.Join(home, ".local", "state")
			}
			return analysis.Export(ctx, filepath.Join(root, "unreal-agent", "exports"), format, report)
		},
	})
}
