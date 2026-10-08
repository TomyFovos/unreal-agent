//go:build linux || darwin

package main

import (
	"bytes"
	"context"
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

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
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
		if !reflect.DeepEqual(ids, []string{"claude-code", "openai-codex"}) {
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
	if config.Providers["claude-code"].ClaudeCode.ToolBridge.ResolvedMode() != claudecode.BridgeModeStructured {
		t.Fatal("new normal bootstrap did not select structured explicitly")
	}
	if config.Permissions.ProcessMode != permission.ProcessDenied || config.Permissions.FilesystemUnrestricted || config.Permissions.NetworkUnrestricted || len(config.Permissions.WriteRoots) != 0 || slices.Contains(config.Permissions.Tools, "Bash") {
		t.Fatal("smoke permission profile became the default")
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

func TestNormalStructuredRegistrationDoesNotRequireMCPGrant(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-mcp-grant", true: "serializer-denied"}[blocked], func(t *testing.T) {
			f, cli := normalFixture(t, true, true)
			f.config.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{f.config.Runtime.Workspace}, NetworkOrigins: f.config.Permissions.NetworkOrigins}
			f.config.Launcher = &normalLauncherConfig{Version: 1, ClaudeCode: &claudecode.Config{Binary: cli.Binary, ManagedPolicyMode: "trust", ToolBridge: claudecode.ToolBridgeConfig{Enabled: true, Mode: claudecode.BridgeModeStructured}}}
			f.writeConfig(t)
			if err := os.WriteFile(filepath.Join(f.config.Runtime.Workspace, "normal-read.txt"), []byte("NORMAL_STRUCTURED_RECEIPT"), 0600); err != nil {
				t.Fatal(err)
			}
			cli.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeManagedPermissionsOnly: true, StructuredInitializeError: blocked, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"normal-read.txt"}`, Contains: "NORMAL_STRUCTURED_RECEIPT"}}})
			l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
				v, err := c.Inspect(ctx, id, 0, 256)
				if err != nil {
					return err
				}
				out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
				if err != nil || len(out.Providers) != 2 {
					t.Fatal("multi-provider registration lost", err)
				}
				for _, p := range out.Providers {
					if p.ID == "claude-code" {
						expected := "available"
						if blocked {
							expected = "blocked_by_policy"
						}
						if p.Availability != "available" || p.ToolBridge != expected || p.Tools == blocked {
							t.Fatal("MCP policy affected structured registration", p.Availability, p.ToolBridge, p.Tools)
						}
					}
				}
				multiSelect(t, c, v, 0, "claude-code", "discovered-b", "high")
				submitInteractive(t, c, v, "structured-normal-input", "Exercise the normal fake Claude runtime.")
				end := inspectInteractive(t, c, id, func(v host.View) bool {
					if v.Failure != "" {
						t.Fatal(v.Failure)
					}
					if blocked {
						return responseCount(v) == 1
					}
					return hasBridgeFinal(v)
				})
				if blocked {
					if len(end.Operations) != 0 {
						t.Fatal("denied serializer caused execution")
					}
				} else {
					if len(end.Operations) != 1 || multiReport(t, end).ToolBridgeMode != "structured" {
						t.Fatal("normal structured registry/Operation path lost")
					}
				}
				return nil
			}}
			if err := l.run(t.Context(), []string{"normal-structured"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !blocked && len(cli.StructuredProbes(t)) == 0 {
				t.Fatal("normal launcher did not use official non-inference schema probe")
			}
		})
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
		{"claude-only", false, true, "", "claude-code", ""},
		{"both-explicit-claude", true, true, "claude-code", "claude-code", ""},
		{"neither", false, false, "", "", "providers_unavailable"},
		{"explicit-unavailable-no-fallback", true, false, "claude-code", "", "initial_provider_unavailable"},
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
				want := 1
				if tc.codex && tc.claude {
					want = 2
				}
				if err != nil || len(p.Providers) != want {
					t.Fatal("wrong provider count", err)
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
	var failure *configurationError
	if _, err := resolveLauncherConfig(t.Context(), o, nil); !errors.As(err, &failure) || failure.Code != "invalid_runtime_configuration" {
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
		if err := l.run(t.Context(), args, io.Discard); err == nil || !strings.Contains(err.Error(), "Host configuration does not match") {
			t.Fatal("incompatible Host reused", err)
		}
	}
	if f.starts.Load() != 1 {
		t.Fatal("incompatible reuse launched second Host")
	}
}

func TestNormalClaudePolicyCapabilitySplitAndTextTurn(t *testing.T) {
	for _, mode := range []string{"blocked_by_policy", "available", "disabled", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f, cli := normalFixture(t, false, true)
			cfg := claudeServeConfiguration(t, cli)
			// Restore the launcher's home so default paths remain private/short.
			t.Setenv("HOME", filepath.Dir(filepath.Dir(filepath.Dir(f.o.config))))
			cfg.Permissions = permission.Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{cfg.Runtime.Workspace}}
			cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: new(false)}
			cfg.Runtime.ClaudeCode.ManagedPolicyMode = "trust"
			cfg.Runtime.ClaudeCode.ToolBridge.Enabled = mode != "disabled"
			rules := []map[string]any{}
			if mode == "available" {
				for _, name := range cfg.Permissions.Tools {
					rules = append(rules, map[string]any{"behavior": "allow", "source": "policySettings", "rule": "mcp__unreal__unreal_" + name})
				}
			}
			rawRules, _ := json.Marshal(rules)
			fake := testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeManagedPermissionsOnly: true, BridgePermissionRules: string(rawRules)}
			if mode == "available" {
				target := filepath.Join(cfg.Runtime.Workspace, "normal-read.txt")
				if err := os.WriteFile(target, []byte("NORMAL_READ_OWNER"), 0600); err != nil {
					t.Fatal(err)
				}
				args, _ := json.Marshal(map[string]string{"path": target})
				fake.BridgeSteps = []testclaude.BridgeStep{{Name: "read", Arguments: string(args), Contains: "NORMAL_READ_OWNER"}}
			}
			if mode == "unknown" {
				fake.BridgeSource = "project"
			}
			cli.Set(t, fake)
			f.config = cfg
			f.writeConfig(t)
			var opened host.View
			l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
				v, err := c.Inspect(ctx, id, 0, 256)
				if err != nil || !v.Running {
					t.Fatal("policy blocked normal Host stopped", err)
				}
				opened = v
				p, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
				if err != nil || len(p.Providers) != 1 || p.Providers[0].Availability != "available" || p.Providers[0].ToolBridge != mode || p.Providers[0].Tools != (mode == "available") {
					t.Fatalf("wrong policy capability: %+v, %v", p.Providers, err)
				}
				if len(v.Operations) != 0 || responseCount(v) != 0 || len(cli.Calls(t)) != 0 {
					t.Fatal("registration executed work/inference")
				}
				return nil
			}}
			if err := l.run(t.Context(), []string{"team"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			if mode == "available" {
				c := gateway.NewClient(f.o.socket)
				defer c.Close()
				submitInteractive(t, c, opened, "native-read", "Use the Unreal read tool")
				done := inspectInteractive(t, c, "team", func(v host.View) bool { return hasBridgeFinal(v) })
				if len(done.Operations) != 1 || done.Operations[0].ToolName != "read" || done.Operations[0].Status != operation.StatusCompleted {
					t.Fatal("normal grant did not use Unreal Operation")
				}
				calls := cli.Calls(t)
				if len(calls) != 1 {
					t.Fatal("duplicate native bridge execution")
				}
				cliArgument(t, calls[0], "--tools", "")
				cliArgument(t, calls[0], "--allowedTools", "mcp__unreal__unreal_read,mcp__unreal__unreal_grep,mcp__unreal__unreal_glob")
				return
			} // previous bridge integration exercises granted execution
			c := gateway.NewClient(f.o.socket)
			defer c.Close()
			health, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "runtime.health", ID: "team"})
			if err != nil || health.Capabilities["claude-code"].ToolBridge != mode {
				t.Fatal("safe runtime status unavailable", err)
			}
			// Only the native fake runs. The real Host -> Coordinator -> provider
			// path must construct Tools=[] while retaining requested config.
			submitInteractive(t, c, opened, "text", "Reply as text using no tools")
			done := inspectInteractive(t, c, "team", func(v host.View) bool { return responseCount(v) == 1 })
			calls := cli.Calls(t)
			if len(calls) != 1 || len(done.Operations) != 0 {
				t.Fatal("text capability created tools or duplicate inference")
			}
			for flag, value := range map[string]string{"--tools": "", "--disallowedTools": "*", "--mcp-config": `{"mcpServers":{}}`} {
				cliArgument(t, calls[0], flag, value)
			}
			if !slices.Contains(calls[0].Arguments, "--safe-mode") || !slices.Contains(calls[0].Arguments, "--strict-mcp-config") {
				t.Fatal("normal text isolation weakened")
			}
			assertNoCredentialLeak(t, calls[0].Input, "launcher-token-sensitive", "launcher-account-sensitive")
			for _, item := range done.History.Items {
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == "configuration" && mode != "disabled" && !sessionstore.ToolBridgeEnabledFromConfiguration(record.Configuration) {
					t.Fatal("effective health rewrote immutable desired bridge configuration")
				}
			}
		})
	}
}

func TestNormalBridgeRegistrationWithExistingChildTemplatesIsInert(t *testing.T) {
	f, cli := normalFixture(t, true, true)
	codex := f.config.Runtime
	cfg := claudeServeConfiguration(t, cli)
	t.Setenv("HOME", filepath.Dir(filepath.Dir(filepath.Dir(f.o.config))))
	cfg.Runtime.ClaudeCode.ManagedPolicyMode = "trust"
	cfg.Runtime.ClaudeCode.ToolBridge.Enabled = true
	cfg.Launcher = &normalLauncherConfig{Version: 1, DiscoverProviders: new(false)}
	cfg.Providers = map[string]agentrunner.ProviderRuntime{"openai-codex": {Provider: codex.Provider, ReasoningEffort: codex.ReasoningEffort}}
	exposed := []string{"read", "SubagentStart", "SubagentSend", "SubagentCancel"}
	// Existing child launch policy requires all three explicit process grants.
	// This reviewed fixture is not the conservative bootstrap default.
	cfg.Permissions = permission.Config{Tools: append(slices.Clone(exposed), "Finish"), ReadRoots: []string{cfg.Runtime.Workspace}, NetworkOrigins: []string{codex.Provider.Endpoint}, ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true}
	cfg.Subagents = map[string]childTemplate{"worker": {Runtime: cfg.Runtime, Permissions: permission.Config{Tools: []string{"Finish", "read"}, ReadRoots: []string{cfg.Runtime.Workspace}}}}
	rules := []map[string]any{}
	for _, name := range exposed {
		rules = append(rules, map[string]any{"behavior": "allow", "source": "policySettings", "rule": "mcp__unreal__unreal_" + name})
	}
	raw, _ := json.Marshal(rules)
	cli.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeManagedPermissionsOnly: true, BridgePermissionRules: string(raw)})
	f.config = cfg
	f.writeConfig(t)
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		v, err := c.Inspect(ctx, id, 0, 256)
		if err != nil {
			return err
		}
		health, err := modelExchange(ctx, c.Extension, modelRequest{Action: "runtime.health"})
		if err != nil || health.Capabilities["claude-code"].ToolBridge != "available" || !health.Capabilities["claude-code"].Tools {
			t.Fatal("child schemas prevented bridge registration", err)
		}
		if len(v.Operations) != 0 || len(cli.Calls(t)) != 0 || responseCount(v) != 0 || f.calls.Load() != 0 {
			t.Fatal("registration started a child, Operation or inference")
		}
		return nil
	}}
	if err := l.run(t.Context(), []string{"normal-child-registry"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	sessions, err := os.ReadDir(f.o.sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range sessions {
		if strings.HasPrefix(entry.Name(), "child-") || strings.Contains(entry.Name(), "provider-registration") {
			t.Fatal("registration created a durable child/Session")
		}
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
	f.config.Launcher = &normalLauncherConfig{Version: 1, ClaudeCode: &claudecode.Config{Binary: cli.Binary, ManagedPolicyMode: "trust", ToolBridge: claudecode.ToolBridgeConfig{Enabled: true}}}
	f.writeConfig(t)
	cli.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeManagedPermissionsOnly: true})
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
			if p.ID == "claude-code" && (p.Availability != "available" || p.Tools || p.ToolBridge != "blocked_by_policy") {
				t.Fatal("policy denial killed text capability")
			}
		}
		multiSelect(t, c, v, 0, "claude-code", "discovered-b", "high")
		submitInteractive(t, c, v, "selected-text", "Use the fake selected Claude runtime")
		before = inspectInteractive(t, c, id, func(v host.View) bool {
			return responseCount(v) == 1 && multiReport(t, v).Selection.Provider == "claude-code"
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
		if selection == nil || selection.Provider != "claude-code" || selection.Model != "discovered-b" || selection.Effort != "high" || selection.Revision != 1 {
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
	f.config.Launcher.ClaudeCode = &claudecode.Config{Binary: cli.Binary, ManagedPolicyMode: "trust"}
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
