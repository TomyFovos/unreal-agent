package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

// Only closed codes cross the discovery/UI boundary. Error bodies, executable
// output, credential material and model/source paths are never retained here.
func providerFailureReason(err error, docker bool) modelcatalog.AvailabilityReason {
	var ca *claudeagent.Error
	var ae *credential.Error
	var ne *normalRuntimeError
	var pe *openaicodex.SandboxProxyError
	switch {
	case errors.As(err, &ca):
		switch ca.Code {
		case "cli_missing":
			return modelcatalog.CLIMissing
		case "external_reauth_required":
			return modelcatalog.AuthRequired
		case "catalog_unavailable":
			return modelcatalog.CatalogUnavailable
		}
	case errors.As(err, &pe):
		return modelcatalog.ProxyInvalid
	case errors.As(err, &ae):
		if docker {
			return modelcatalog.ProxyInvalid
		}
		return modelcatalog.AuthRequired
	case errors.As(err, &ne):
		if ne.Code == "catalog_unavailable" {
			return modelcatalog.CatalogUnavailable
		}
	}
	if docker {
		return modelcatalog.ProxyInvalid
	}
	return modelcatalog.ProviderCheckFailed
}

func unavailableProvider(id string, reason modelcatalog.AvailabilityReason) modelcatalog.Provider {
	return modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: "unavailable", Reason: reason}
}

func bootstrapDiscovery(o launchOptions) serveConfiguration {
	return serveConfiguration{Launcher: &normalLauncherConfig{Version: 1, CodexDockerSandboxOAuth: o.bootstrapDocker,
		CodexAuto: o.bootstrapAuto, CodexDisabled: o.bootstrapCodexDisabled, ClaudeDisabled: o.bootstrapClaudeDisabled, ClaudeBinary: o.bootstrapClaude}}
}

func validateBootstrapChoices(o launchOptions) error {
	if o.bootstrapDocker && (o.bootstrapCodexDisabled || o.bootstrapAuto) || o.bootstrapAuto && o.bootstrapCodexDisabled {
		return errors.New("conflicting provider configuration; no discovery started")
	}
	return nil
}

// Authentication and catalog only: no Host, Session, thread, user prompt,
// inference, bridge initialization or executor is created by this command.
func runProviderPreflight(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("unreal-agent providers", flag.ContinueOnError)
	flags.SetOutput(out)
	var o launchOptions
	flags.BoolVar(&o.bootstrapDocker, "docker-sandbox-oauth", false, "explicit Docker OAuth route")
	flags.BoolVar(&o.bootstrapAuto, "codex-auto", false, "native validated Codex source discovery; no route fallback")
	flags.BoolVar(&o.bootstrapCodexDisabled, "codex-disabled", false, "explicitly decline Codex discovery")
	flags.BoolVar(&o.bootstrapClaudeDisabled, "claude-disabled", false, "explicitly decline Claude discovery")
	flags.StringVar(&o.bootstrapClaude, "claude-binary", "", "external Claude executable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("provider configuration confirmation invalid; no process started")
	}
	if err := validateBootstrapChoices(o); err != nil {
		return err
	}
	cfg, err := registerNormalProviders(ctx, bootstrapDiscovery(o), "", os.Getenv)
	if err != nil {
		return err
	}
	// Only the automatic-discovery opt-in extends the old machine contract.
	// Failed discovery stays auto; it must never silently choose another source.
	route := ""
	if o.bootstrapAuto {
		route = "auto"
		if p, ok := cfg.Providers["openai-codex"]; ok {
			route = "external"
			if isDockerSandboxCodex(p.Provider.Auth) {
				route = "docker"
			}
		}
	}
	return json.MarshalWrite(out, struct {
		Providers  []modelcatalog.Provider
		CodexRoute string `json:",omitzero"`
	}{cfg.providerStatuses, route})
}
