//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/internal/privateexport"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

const normalCodexCache = `{"models":[{"slug":"gpt-6.1-sol","display_name":"Synthetic GPT","visibility":"list","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`

func normalFixture(t *testing.T, codex, claude bool) (*launcherFixture, *testclaude.Fixture) {
	t.Helper()
	f := newLauncherFixture(t)
	cli := testclaude.New(t)
	t.Chdir(privateCLIDirectory(t))
	cli.Set(t, testclaude.Config{SignedOut: !claude, Catalog: hostClaudeCatalog})
	if err := os.WriteFile(filepath.Join(cli.Directory, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli.Directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("UNREAL_HARNESS_LLM_PROVIDER", "")
	t.Setenv("UNREAL_HARNESS_LLM_MODEL", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if !codex {
		if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".codex", "models_cache.json"), []byte(normalCodexCache), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.o.config); err != nil {
		t.Fatal(err)
	}
	return f, cli
}

func TestNormalLauncherFirstRunTwoProvidersDefaultPathsReuseAndResume(t *testing.T) {
	f, cli := normalFixture(t, true, true)
	var choices int
	l := launcher{start: f.start, choose: func(ids []string) (string, error) {
		choices++
		if !reflect.DeepEqual(ids, []string{claudeagent.ProviderID, "openai-codex"}) {
			t.Fatal("unstable initial choices", ids)
		}
		return "openai-codex", nil
	}}
	var before host.View
	l.attach = func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, err := c.Inspect(ctx, id, 0, 256)
		if err != nil || !v.Running {
			t.Fatal("normal session not running", err)
		}
		providers, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
		if err != nil || len(providers.Providers) != 2 {
			t.Fatal("normal Host missing multiple providers", err)
		}
		for _, p := range providers.Providers {
			if p.Availability != "available" || !p.Tools || p.ID == "claude-code" && p.ToolBridge != "available" {
				t.Fatalf("wrong registration: %s %s %s tools=%v", p.ID, p.Availability, p.ToolBridge, p.Tools)
			}
		}
		if id == "dev" {
			if before.Generation != "" && before.Generation != v.Generation {
				t.Fatal("normal reconnect resumed a running session")
			}
			before = v
		}
		return nil
	}
	for _, args := range [][]string{nil, {"dev"}, {"dev"}} {
		if err := l.run(t.Context(), args, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if choices != 1 || f.starts.Load() != 1 || f.calls.Load() != 0 || len(cli.Calls(t)) != 0 {
		t.Fatal("startup selected twice, duplicated Host, or sent inference")
	}
	config, err := readServeConfiguration(f.o.config)
	if err != nil || config.Runtime.Provider.Provider != "openai-codex" || len(config.Providers) != 1 {
		t.Fatal("bootstrap not persisted", err)
	}
	if claude := config.Providers[claudeagent.ProviderID].Claude; claude == nil || claude.Binary != cli.Binary {
		t.Fatal("new normal bootstrap did not register the Claude Code route (U21) with its CLI")
	}
	if config.Permissions.ProcessMode != permission.ProcessDenied || config.Permissions.FilesystemUnrestricted || config.Permissions.NetworkUnrestricted || len(config.Permissions.WriteRoots) != 0 || slices.Contains(config.Permissions.Tools, "Bash") {
		t.Fatal("smoke permission profile became the default")
	}
	if config.PermissionModes == nil || !config.PermissionModes.Enabled || config.PermissionModes.Maximum == nil || !slices.Contains(config.PermissionModes.Maximum.Tools, "Bash") {
		t.Fatal("bootstrap lost human-controlled capability ceiling")
	}
	for path, want := range map[string]fs.FileMode{f.o.config: 0600, filepath.Dir(f.o.config): 0700, f.o.state: 0700, f.o.sessions: 0700, filepath.Dir(f.o.socket): 0700} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("unsafe default artifact", path, err)
		}
	}
	data, _ := os.ReadFile(f.o.config)
	assertNoCredentialLeak(t, string(data), "launcher-token-sensitive", "launcher-account-sensitive", "ambient-launcher-token-sensitive", "ambient-launcher-account-sensitive")
	c := gateway.NewClient(f.o.socket)
	defer c.Close()
	stopMultiSession(t, c, before)
	l.attach = func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, e := c.Inspect(ctx, id, 0, 256)
		selection := multiReport(t, v).Selection
		if e != nil || !v.Running || v.Generation == before.Generation || selection == nil || selection.Provider != "openai-codex" {
			t.Fatal("stopped normal session did not resume its own runtime", e)
		}
		return nil
	}
	if err = l.run(t.Context(), []string{"dev"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.o.config)
	if !bytes.Equal(data, after) || f.starts.Load() != 1 || len(cli.Calls(t)) != 0 {
		t.Fatal("resume changed configuration, duplicated Host or inferred")
	}
}

func TestNormalLauncherFirstRunProviderAvailabilityAndExplicitInitialOverride(t *testing.T) {
	for _, tc := range []struct {
		name              string
		codex, claude     bool
		override, initial string
		failure           string
	}{
		{"codex-only", true, false, "", "openai-codex", ""},
		{"claude-only", false, true, "", claudeagent.ProviderID, ""},
		{"both-explicit-claude", true, true, claudeagent.ProviderID, claudeagent.ProviderID, ""},
		{"neither", false, false, "", "", "providers_unavailable"},
		{"explicit-unavailable-no-fallback", true, false, claudeagent.ProviderID, "", "initial_provider_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cli := normalFixture(t, tc.codex, tc.claude)
			t.Setenv("UNREAL_HARNESS_LLM_PROVIDER", tc.override)
			l := launcher{start: f.start, choose: func([]string) (string, error) {
				t.Fatal("sole provider or explicit override unexpectedly prompted")
				return "", nil
			}, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
				v, err := c.Inspect(ctx, id, 0, 256)
				selection := multiReport(t, v).Selection
				if err != nil || selection == nil || selection.Provider != tc.initial {
					t.Fatal("wrong initial provider", err)
				}
				p, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
				if err != nil || len(p.Providers) != 2 {
					t.Fatal("supported provider missing", err)
				}
				for _, entry := range p.Providers {
					wantAvailable := tc.codex && entry.ID == "openai-codex" || tc.claude && entry.ID == claudeagent.ProviderID
					if (entry.Availability == "available") != wantAvailable || !wantAvailable && (entry.Reason == "" || entry.Reason.Hint() == "" || len(entry.Catalog.Models) != 0) {
						t.Fatal("availability or safe reason incorrect")
					}
				}
				return nil
			}}
			err := l.run(t.Context(), []string{"normal"}, io.Discard)
			if tc.failure != "" {
				var failure *normalRuntimeError
				if !errors.As(err, &failure) || failure.Code != tc.failure || f.starts.Load() != 0 {
					t.Fatal("unexpected safe startup failure", err)
				}
				if _, e := os.Stat(f.o.config); !errors.Is(e, fs.ErrNotExist) {
					t.Fatal("failed discovery published config")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(cli.Calls(t)) != 0 || f.calls.Load() != 0 {
				t.Fatal("startup inferred")
			}
		})
	}
}

func TestNormalLauncherConcurrentBootstrapNoOverwriteAndMalformedConfiguration(t *testing.T) {
	f, cli := normalFixture(t, true, false)
	var group sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		group.Go(func() {
			l := launcher{start: f.start, attach: func(context.Context, *gateway.Client, session.ID) error { return nil }}
			errs <- l.run(t.Context(), []string{"concurrent"}, io.Discard)
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.starts.Load() != 1 || len(cli.Calls(t)) != 0 {
		t.Fatal("concurrent bootstrap duplicated Host or inferred")
	}
	before, _ := os.ReadFile(f.o.config)
	if err := privateexport.CreateRuntimeConfig(t.Context(), f.o.config, []byte("replacement")); !errors.Is(err, fs.ErrExist) {
		t.Fatal("config overwritten", err)
	}
	after, _ := os.ReadFile(f.o.config)
	if !bytes.Equal(before, after) {
		t.Fatal("existing config changed")
	}
	bad := filepath.Join(filepath.Dir(f.o.config), "malformed.json")
	if err := os.WriteFile(bad, []byte(`{"Runtime":`), 0600); err != nil {
		t.Fatal(err)
	}
	o := f.o
	o.config, o.normal = bad, true
	var failure *jsontext.SyntacticError
	if _, err := resolveLauncherConfig(t.Context(), o, nil); !errors.As(err, &failure) {
		t.Fatal("malformed config bootstrapped", err)
	}
	badData, _ := os.ReadFile(bad)
	if string(badData) != `{"Runtime":` {
		t.Fatal("malformed config replaced")
	}
}

func TestNormalLauncherMissingCatalogDoesNotGuessModelsOrCredentials(t *testing.T) {
	f, cli := normalFixture(t, true, false)
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".codex", "models_cache.json")); err != nil {
		t.Fatal(err)
	}
	var failure *normalRuntimeError
	err := (launcher{start: f.start}).run(t.Context(), nil, io.Discard)
	if !errors.As(err, &failure) || failure.Code != "catalog_unavailable" || f.starts.Load() != 0 {
		t.Fatal("missing catalog was guessed or misclassified", err)
	}
	if _, err := os.Stat(f.o.config); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing catalog published configuration")
	}
	if len(cli.Calls(t)) != 0 || f.calls.Load() != 0 {
		t.Fatal("catalog absence led to inference")
	}
}

func TestNormalBootstrapDoesNotGrantReadsOfPrivateWorkspace(t *testing.T) {
	for _, kind := range []string{"home", "codex", "host-state", "home-alias"} {
		t.Run(kind, func(t *testing.T) {
			f, cli := normalFixture(t, true, false)
			home := os.Getenv("HOME")
			workspace := home
			switch kind {
			case "codex":
				workspace = filepath.Join(home, ".codex")
			case "host-state":
				workspace = f.o.state
				if err := os.MkdirAll(workspace, 0700); err != nil {
					t.Fatal(err)
				}
			case "home-alias":
				workspace = filepath.Join(privateCLIDirectory(t), "linked-home")
				if err := os.Symlink(home, workspace); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(workspace)
			l := launcher{start: f.start, attach: func(context.Context, *gateway.Client, session.ID) error { return nil }}
			if err := l.run(t.Context(), nil, io.Discard); err != nil {
				t.Fatal("private working directory prevented text startup", err)
			}
			config, err := readServeConfiguration(f.o.config)
			if err != nil || len(config.Permissions.ReadRoots) != 0 || len(config.Permissions.Tools) != 0 || config.Permissions.FilesystemUnrestricted || config.Permissions.NetworkUnrestricted || config.Permissions.ProcessMode != permission.ProcessDenied {
				t.Fatal("private startup directory received automatic capabilities", err)
			}
			if len(cli.Calls(t)) != 0 || f.calls.Load() != 0 {
				t.Fatal("private workspace detection sent inference")
			}
		})
	}
}

func TestNormalLauncherPreservesExistingConfigAndRejectsIncompatibleHostReuse(t *testing.T) {
	f, cli := normalFixture(t, true, true)
	f.writeConfig(t) // legacy single-provider operator config; remains byte-for-byte unchanged
	before, _ := os.ReadFile(f.o.config)
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, err := c.Inspect(ctx, id, 0, 256)
		selection := multiReport(t, v).Selection
		if err != nil || selection == nil || selection.Provider != f.config.Runtime.Provider.Provider || selection.Model != f.config.Runtime.Provider.Model.ID || selection.Effort != string(f.config.Runtime.ReasoningEffort) {
			t.Fatal("explicit startup runtime changed", err)
		}
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
		if err != nil || len(out.Providers) != 2 {
			t.Fatal("legacy default config did not gain independent registration", err)
		}
		return nil
	}}
	if err := l.run(t.Context(), []string{"existing"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.o.config)
	if !bytes.Equal(before, after) || len(cli.Calls(t)) != 0 {
		t.Fatal("normal discovery rewrote config or inferred")
	}
	for _, args := range [][]string{{"--session-directory", filepath.Join(f.o.state, "other-sessions")}, {"--config", f.o.config}} {
		if err := l.run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "別の設定で起動中") {
			t.Fatal("incompatible Host reused", err)
		}
	}
	if f.starts.Load() != 1 {
		t.Fatal("incompatible reuse launched second Host")
	}
}

func TestNormalLauncherExplicitOverridesDoNotReadOrOverwriteDefaultConfig(t *testing.T) {
	f, cli := normalFixture(t, true, true)
	if err := os.WriteFile(f.o.config, []byte(`{"invalid-default":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(privateCLIDirectory(t), "explicit.json")
	data, _ := json.Marshal(f.config)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(privateCLIDirectory(t), "state")
	socket := filepath.Join(privateCLIDirectory(t), "custom.sock")
	var args launchOptions
	l := launcher{start: func(o launchOptions) (*backgroundHost, error) { args = o; return f.start(o) }, attach: func(context.Context, *gateway.Client, session.ID) error { return nil }}
	if err := l.run(t.Context(), []string{"explicit", "--config", configPath, "--state-directory", state, "--socket", socket}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if args.config != configPath || args.state != state || args.sessions != filepath.Join(state, "sessions") || args.socket != socket || args.normal {
		t.Fatal("explicit override lost")
	}
	after, _ := os.ReadFile(f.o.config)
	if string(after) != `{"invalid-default":true}` || len(cli.AuthCalls(t)) != 0 || len(cli.Calls(t)) != 0 {
		t.Fatal("explicit config was replaced/merged with normal discovery")
	}
}

func TestNormalDiscoveryTeamTrustAndRestartRetainsCanonicalSelection(t *testing.T) {
	f, cli := normalFixture(t, true, true)
	f.config.Permissions.Tools = []string{"read", "grep", "glob"}
	f.config.Permissions.ReadRoots = []string{f.config.Runtime.Workspace}
	f.config.Launcher = &normalLauncherConfig{Version: 1, ClaudeBinary: cli.Binary}
	f.writeConfig(t)
	cli.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog})
	var before host.View
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, err := c.Inspect(ctx, id, 0, 256)
		if err != nil {
			return err
		}
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
		if err != nil || len(out.Providers) != 2 {
			t.Fatal("normal trust discovery missing providers", err)
		}
		for _, p := range out.Providers {
			if p.ID == claudeagent.ProviderID && (p.Availability != "available" || !p.Tools) {
				t.Fatal("the Claude Code route was not registered from the Team login", p.Availability, p.Tools)
			}
		}
		multiSelect(t, c, v, 0, claudeagent.ProviderID, "discovered-b", "high")
		submitInteractive(t, c, v, "selected-text", "Use the fake selected Claude runtime")
		before = inspectInteractive(t, c, id, func(v host.View) bool {
			return responseCount(v) == 1 && multiReport(t, v).Selection.Provider == claudeagent.ProviderID
		})
		return nil
	}}
	if err := l.run(t.Context(), []string{"restore-provider"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	child := f.children[0]
	f.mu.Unlock()
	if err := child.process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.done:
	case <-time.After(5 * time.Second):
		t.Fatal("normal Host did not stop")
	}
	l.attach = func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, err := c.Inspect(ctx, id, 0, 256)
		if err != nil {
			return err
		}
		selection := multiReport(t, v).Selection
		if selection == nil || selection.Provider != claudeagent.ProviderID || selection.Model != "discovered-b" || selection.Effort != "high" || selection.Revision != 1 {
			t.Fatal("restart overwrote canonical runtime with startup Codex")
		}
		if len(v.History.Items) < len(before.History.Items) || !reflect.DeepEqual(v.History.Items[:len(before.History.Items)], before.History.Items) {
			t.Fatal("restart rewrote canonical history")
		}
		return nil
	}
	if err := l.run(t.Context(), []string{"restore-provider"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.starts.Load() != 2 || len(cli.Calls(t)) != 1 || f.calls.Load() != 0 {
		t.Fatal("restart inferred or duplicated provider")
	}
}

func TestNormalRegisteredUnavailableInitialProviderNeverAutomaticallyFallsBack(t *testing.T) {
	f, cli := normalFixture(t, false, true)
	f.config.Launcher = &normalLauncherConfig{Version: 1}
	f.config.Launcher.ClaudeBinary = cli.Binary
	f.writeConfig(t) // initial Codex retained even though only Claude is healthy
	var v host.View
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, _ = c.Inspect(ctx, id, 0, 256)
		return nil
	}}
	if err := l.run(t.Context(), []string{"no-fallback"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	c := gateway.NewClient(f.o.socket)
	defer c.Close()
	submitInteractive(t, c, v, "unavailable", "must fail at selected Codex, not go to Claude")
	failed := inspectInteractive(t, c, "no-fallback", func(v host.View) bool { return !v.Running })
	if failed.Failure == "" || responseCount(failed) != 0 || len(cli.Calls(t)) != 0 || f.calls.Load() != 0 || multiReport(t, failed).Selection.Provider != "openai-codex" {
		t.Fatal("unavailable provider silently fell back")
	}
}
