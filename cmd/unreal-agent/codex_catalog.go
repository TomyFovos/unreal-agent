package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

// The process source is explicit and proxy-only. A native external token, an
// API key, a placeholder alone or a failed provider never selects this route.
func codexCatalogReader(auth string, sandbox bool, getenv func(string) string) func(context.Context, bool) (modelcatalog.Catalog, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if !sandbox {
		return func(context.Context, bool) (modelcatalog.Catalog, error) {
			return modelcatalog.ReadCodexCache(codexModelCache(auth)), nil
		}
	}
	cli := &modelcatalog.CodexCLI{Home: filepath.Dir(auth), Getenv: getenv, Validate: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return (openaicodex.DockerSandboxSource{AuthFile: auth, Getenv: getenv}).Validate()
	}}
	return func(ctx context.Context, refresh bool) (modelcatalog.Catalog, error) {
		return cli.Catalog(ctx, refresh), nil
	}
}
