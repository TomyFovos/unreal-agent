package agentrunner

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"io"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type Config struct {
	// Context selects the rebuildable strategy; zero uses bounded v1 defaults.
	Context      contextengine.Config
	Name         string
	Providers    []Provider
	ParseRequest func(io.Reader) (Request, ToolFactory, error)
}

type ToolConfig struct {
	Directory              string
	MutationStateDirectory string
	Translators            tool.StaticTranslators
	Names                  []string
	SessionID              session.ID
	Getenv                 func(string) string
	// CatalogOnly composes inert schemas for a private registration probe.
	// No owner-bound subagent manager may be created by this composition.
	CatalogOnly bool
}

type Tools struct {
	Registry   tool.Registry
	RemoteJobs []operation.RemoteJobHandler
	Close      func() error
}

type ToolFactory func(context.Context, ToolConfig) (Tools, error)

func (parsed Request) EnabledTools(names ...string) []string {
	enabled := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(parsed.DisallowedTools, name) {
			enabled = append(enabled, name)
		}
	}
	return enabled
}
