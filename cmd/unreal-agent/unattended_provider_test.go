//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestInstallConsentNativePreflightAndBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		docker, badProxy, noCodex, noClaude, noCatalog, missingCLI bool
	}{
		{name: "external-and-team"}, {name: "docker-and-team", docker: true},
		{name: "bad-docker-keeps-claude", docker: true, badProxy: true},
		{name: "expired-codex-keeps-claude", noCodex: true}, {name: "missing-cli-keeps-codex", missingCLI: true},
		{name: "missing-catalog-keeps-claude", noCatalog: true}, {name: "both-unavailable", noCodex: true, noClaude: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cli := normalFixture(t, !tc.noCodex, !tc.noClaude)
			cli.Set(t, testclaude.Config{Subscription: "team", SignedOut: tc.noClaude, Catalog: hostClaudeCatalog})
			t.Setenv("SBX_CRED_OPENAI_MODE", "")
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("https_proxy", "")
			if tc.docker {
				p := sandboxCodexFixture(t)
				t.Setenv("CODEX_HOME", filepath.Dir(p))
				t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
				proxy := "http://host.docker.internal:3128"
				if tc.badProxy {
					proxy = "http://PRIVATE_VALUE@evil.invalid:3128"
				}
				t.Setenv("HTTPS_PROXY", proxy)
			}
			if tc.noCatalog {
				os.Remove(filepath.Join(os.Getenv("HOME"), ".codex/models_cache.json"))
			}
			binary := cli.Binary
			if tc.missingCLI {
				binary = filepath.Join(cli.Directory, "absent")
			}
			args := []string{"--codex-auto", "--claude-binary", binary}
			var out bytes.Buffer
			if err := runProviderPreflight(t.Context(), args, &out); err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Providers  []modelcatalog.Provider
				CodexRoute string
			}
			if json.Unmarshal(out.Bytes(), &reply, json.RejectUnknownMembers(true)) != nil || len(reply.Providers) != 2 {
				t.Fatal("unsafe status shape")
			}
			codexOK := !tc.noCodex && !tc.noCatalog && !tc.badProxy
			claudeOK := !tc.noClaude && !tc.missingCLI
			for _, p := range reply.Providers {
				want := codexOK
				if p.ID == claudeagent.ProviderID {
					want = claudeOK
				}
				if (p.Availability == "available") != want {
					t.Fatal("unproven availability", p.ID, p.Reason)
				}
			}
			wantRoute := "auto"
			if codexOK {
				wantRoute = "external"
				if tc.docker {
					wantRoute = "docker"
				}
			}
			if reply.CodexRoute != wantRoute || strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "proxy-managed") || len(cli.Calls(t)) != 0 {
				t.Fatal("route fallback, leak or inference")
			}
			o, err := parseLaunchOptions([]string{"--bootstrap-codex-auto", "--bootstrap-claude-binary", binary}, io.Discard, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := bootstrapRuntime(t.Context(), o, func(ids []string) (string, error) { return ids[0], nil }, os.Getenv)
			if !codexOK && !claudeOK {
				if err == nil {
					t.Fatal("unavailable providers started")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Launcher == nil || cfg.Launcher.ClaudeBinary != binary {
				t.Fatal("explicit Claude executable lost")
			}
			if cfg.Permissions.FilesystemUnrestricted || cfg.Permissions.NetworkUnrestricted || strings.Join(cfg.Permissions.Tools, ",") != "read,grep,glob" {
				t.Fatal("installation granted execution rights")
			}
		})
	}
}

func TestInstallConsentRejectsConflictingOptionsAndPreservesExplicitConfig(t *testing.T) {
	_, cli := normalFixture(t, true, true)
	for _, args := range [][]string{
		{"--codex-auto", "--docker-sandbox-oauth"}, {"--codex-auto", "--codex-disabled"},
	} {
		if runProviderPreflight(t.Context(), args, io.Discard) == nil {
			t.Fatal("conflicting consent accepted")
		}
	}
	if len(cli.Calls(t)) != 0 {
		t.Fatal("invalid setup performed inference")
	}
	o, err := parseLaunchOptions(nil, io.Discard, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	// Existing config and creation identity win.
	cfg := bootstrapDiscovery(o)
	cfg.Runtime.Workspace = os.Getenv("HOME")
	data, _ := json.Marshal(cfg)
	p := filepath.Join(t.TempDir(), "runtime.json")
	os.WriteFile(p, data, 0600)
	o, err = parseLaunchOptions([]string{"--config", p, "--bootstrap-claude-binary", cli.Binary}, io.Discard, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveLauncherConfig(t.Context(), o, nil)
	after, _ := os.ReadFile(p)
	if err != nil || !bytes.Equal(after, data) || got.Launcher == nil || got.Launcher.ClaudeBinary != "" {
		t.Fatal("explicit config changed", err)
	}
}
