package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

// claudeBinary finds the claude CLI the user signed in with: a configured
// path first, then PATH, then the native installer's location.
func claudeBinary(config serveConfiguration, getenv func(string) string) (string, error) {
	if p, ok := config.Providers[claudeagent.ProviderID]; ok && p.Claude != nil {
		return p.Claude.Binary, nil
	}
	if config.Runtime.Provider.Provider == claudeagent.ProviderID && config.Runtime.Claude != nil {
		return config.Runtime.Claude.Binary, nil
	}
	if config.Launcher != nil && filepath.IsAbs(config.Launcher.ClaudeBinary) {
		return config.Launcher.ClaudeBinary, nil
	}
	candidates := []string{}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if filepath.IsAbs(dir) {
			candidates = append(candidates, filepath.Join(dir, "claude"))
		}
	}
	if home := getenv("HOME"); filepath.IsAbs(home) {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "claude"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &claudeagent.Error{Code: "cli_missing", Message: "install Claude Code and sign in by running claude"}
}

// discoverClaudeAgent registers the Claude route when Claude Code is signed in
// with a Claude subscription. Discovery reads Claude Code's initialize reply
// only; it starts no conversation (U21, U5).
func discoverClaudeAgent(ctx context.Context, config serveConfiguration, getenv func(string) string) (agentrunner.ProviderRuntime, error) {
	binary, err := claudeBinary(config, getenv)
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	catalog, account, err := claudeagent.Discover(ctx, binary, nil)
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	if !account.Subscription() {
		return agentrunner.ProviderRuntime{}, claudeSignIn()
	}
	model, err := initialCatalogModel(catalog, "opus")
	if err != nil {
		model, err = initialCatalogModel(catalog, "")
	}
	if err != nil {
		return agentrunner.ProviderRuntime{}, err
	}
	return agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: claudeagent.ProviderID, Model: provider.Model{ID: model.ID}, Auth: credential.Reference{Provider: claudeagent.ProviderID, Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 2, Source: "Claude Code initialize"}, ReasoningEffort: model.DefaultEffort, Claude: &claudeagent.Config{Binary: binary}}, nil
}

func claudeSignIn() error {
	return &claudeagent.Error{Code: "external_reauth_required", Message: "sign in to Claude Code with your Claude subscription: run claude, then /login"}
}

// claudeCatalog serves /model from Claude Code's initialize reply, read at most
// once a minute.
type claudeCatalog struct {
	binary string
	mu     sync.Mutex
	at     time.Time
	result modelcatalog.Catalog
	err    error
}

func (c *claudeCatalog) read(ctx context.Context, refresh bool) (modelcatalog.Catalog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refresh || c.at.IsZero() || time.Since(c.at) > time.Minute {
		catalog, account, err := claudeagent.Discover(ctx, c.binary, nil)
		if err == nil && !account.Subscription() {
			err = claudeSignIn()
		}
		c.result, c.err, c.at = catalog, err, time.Now()
	}
	return c.result.Clone(), c.err
}

func (c *claudeCatalog) check(ctx context.Context) error {
	_, err := c.read(ctx, false)
	return err
}
