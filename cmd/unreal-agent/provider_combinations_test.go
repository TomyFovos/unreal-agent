//go:build linux || darwin

package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func assertClaudeDelegation(t *testing.T, cli *testclaude.Fixture, gpt bool) {
	t.Helper()
	calls := cli.Calls(t)
	if len(calls) == 0 {
		t.Fatal("no synthetic Claude turn")
	}
	call := calls[len(calls)-1]
	index := slices.Index(call.Arguments, "--tools")
	if index < 0 || index+1 >= len(call.Arguments) {
		t.Fatal("missing tool allowlist")
	}
	agent := slices.Contains(strings.Split(call.Arguments[index+1], ","), "Agent")
	if agent == gpt || strings.Contains(strings.Join(call.Arguments, "\n"), "Templates:") != gpt {
		t.Fatalf("gpt=%t Agent=%t args=%v", gpt, agent, call.Arguments)
	}
	if !gpt && !slices.Contains(call.Environment, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") {
		t.Fatal("Claude Agent work could outlive its turn")
	}
}

func TestProviderCombinationsNoLoginFirstInputAndLaterLogin(t *testing.T) {
	for _, target := range []string{claudeagent.ProviderID, "openai-codex"} {
		t.Run(target, func(t *testing.T) {
			f, cli := normalFixture(t, false, false)
			o, err := parseLaunchOptions([]string{"--bootstrap-claude-binary", cli.Binary}, io.Discard, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), os.Getenv)
			if err != nil || cfg.Runtime.Provider.Provider != loginProvider {
				t.Fatal("signed-out bootstrap", err)
			}
			client, _, _ := startInteractiveHost(t, cfg)
			view, err := client.Open(t.Context(), host.Create, "login-later")
			if err != nil {
				t.Fatal(err)
			}
			submitInteractive(t, client, view, "first", "help me")
			view = inspectInteractive(t, client, "login-later", func(v host.View) bool { return hasFinalText(v, "codex login") && v.Progress == nil })
			for _, text := range []string{"/login", "/model refresh", "インストールのやり直しは不要"} {
				if !hasFinalText(view, text) {
					t.Fatal("missing first-input guidance", text)
				}
			}
			if target == claudeagent.ProviderID {
				cli.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "new Claude login works"})
			} else {
				writeCodexAuth(t, filepath.Join(os.Getenv("HOME"), ".codex/auth.json"), "synthetic-later-token", "synthetic-account")
			}
			// The bare /model refresh uses the current (local login) selection.
			if _, err = modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.catalog", ID: "login-later", Refresh: true}); err != nil {
				t.Fatal("refresh of login state", err)
			}
			catalog, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.catalog", ID: "login-later", Provider: target})
			if err != nil || catalog.Catalog == nil || !catalog.Catalog.Available {
				t.Fatal("new login not usable", err)
			}
			if f.calls.Load() != 0 || len(cli.Calls(t)) != 0 {
				t.Fatal("login/discovery performed inference")
			}
			model, err := initialCatalogModel(*catalog.Catalog, "")
			if err != nil {
				t.Fatal(err)
			}
			stopMultiSession(t, client, view)
			choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "choose-login", Provider: target, Model: model.ID, Effort: model.DefaultEffort}
			if _, err = modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.select", ID: "login-later", Generation: view.Generation, Selection: choice}); err != nil {
				t.Fatal("cannot select newly signed-in provider", err)
			}
			view, err = client.Open(t.Context(), host.Resume, "login-later")
			if err != nil {
				t.Fatal("cannot resume after login", err)
			}
			// Host applies a pending choice at the next input boundary.
			report := multiReport(t, view)
			recorded := report.Selection.Provider == target
			for _, item := range view.History.Items {
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == sessionstore.HostRuntimeSelection && record.Selection != nil && record.Selection.Provider == target {
					recorded = true
				}
			}
			if !recorded {
				t.Fatal("new selection did not survive resume")
			}
			if target == claudeagent.ProviderID {
				submitInteractive(t, client, view, "after-login", "hello")
				view = inspectInteractive(t, client, "login-later", func(v host.View) bool { return hasFinalText(v, "new Claude login works") && v.Progress == nil })
				if multiReport(t, view).Selection.Provider != target {
					t.Fatal("new login was not applied on input")
				}
				assertClaudeDelegation(t, cli, false)
			}
		})
	}
}

func TestProviderCombinationsClaudeDelegationBeforeAndAfterCodexLogin(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "later-login", true: "stale-config"}[configured], func(t *testing.T) {
			testClaudeDelegationSequence(t, configured)
		})
	}
}

func testClaudeDelegationSequence(t *testing.T, configured bool) {
	t.Helper()
	_, cli := normalFixture(t, configured, true)
	cli.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "synthetic turn complete"})
	o, _ := parseLaunchOptions(nil, io.Discard, os.Getenv)
	cfg, err := bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".codex/auth.json")); err != nil {
			t.Fatal(err)
		}
	}
	client, _, _ := startInteractiveHost(t, cfg)
	view, err := client.Open(t.Context(), host.Create, "delegation")
	if err != nil {
		t.Fatal(err)
	}
	for i, ready := range []bool{false, true, false, true} {
		path := filepath.Join(os.Getenv("HOME"), ".codex/auth.json")
		if ready {
			writeCodexAuth(t, path, "synthetic-later-token", "synthetic-account")
		} else if i > 0 {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.providers", ID: "delegation", Refresh: true}); err != nil {
			t.Fatal(err)
		}
		submitInteractive(t, client, view, string(rune('a'+i)), "hello")
		inspectInteractive(t, client, "delegation", func(v host.View) bool { return responseCount(v) >= i+1 && v.Progress == nil })
		assertClaudeDelegation(t, cli, ready)
	}
}

func TestProviderCombinationsNeitherCLIInstalled(t *testing.T) {
	_, cli := normalFixture(t, false, false)
	t.Setenv("PATH", cli.Directory)
	for _, name := range []string{cli.Binary, filepath.Join(cli.Directory, "codex")} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	o, _ := parseLaunchOptions([]string{"--bootstrap-claude-binary", cli.Binary}, io.Discard, os.Getenv)
	cfg, err := bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), os.Getenv)
	if err != nil || cfg.Runtime.Provider.Provider != loginProvider {
		t.Fatal("missing CLIs prevented startup", err)
	}
	for _, p := range cfg.providerStatuses {
		if p.Availability == "available" || p.Reason != modelcatalog.CLIMissing {
			t.Fatal("missing CLI status", p.ID, p.Reason)
		}
	}
	client, _, _ := startInteractiveHost(t, cfg)
	view, err := client.Open(t.Context(), host.Create, "no-cli")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, client, view, "first", "hello")
	inspectInteractive(t, client, "no-cli", func(v host.View) bool {
		return hasFinalText(v, "CLI がなければ先にインストール") && v.Progress == nil
	})
}

func TestProviderCombinationsStaleConfiguredLoginKeepsSelection(t *testing.T) {
	for _, scenario := range []struct {
		id   string
		both bool
	}{{"openai-codex", false}, {claudeagent.ProviderID, false}, {"openai-codex", true}, {claudeagent.ProviderID, true}} {
		name := scenario.id
		if scenario.both {
			name += "-both-expired"
		}
		t.Run(name, func(t *testing.T) {
			unavailable := scenario.id
			_, cli := normalFixture(t, true, true)
			o, _ := parseLaunchOptions(nil, io.Discard, os.Getenv)
			cfg, err := bootstrapRuntime(t.Context(), o, func([]string) (string, error) { return unavailable, nil }, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			if unavailable == "openai-codex" || scenario.both {
				writeCodexAuth(t, filepath.Join(os.Getenv("HOME"), ".codex/auth.json"), "e30.eyJleHAiOjF9.signature", "synthetic-account")
			}
			if unavailable == claudeagent.ProviderID || scenario.both {
				cli.Set(t, testclaude.Config{SignedOut: true, Catalog: hostClaudeCatalog})
			}
			client, _, _ := startInteractiveHost(t, cfg)
			view, err := client.Open(t.Context(), host.Create, "stale-login")
			if err != nil {
				t.Fatal(err)
			}
			out, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.providers", ID: "stale-login"})
			if err != nil || len(out.Providers) != 2 {
				t.Fatal(err)
			}
			for _, p := range out.Providers {
				if (p.Availability == "available") != (!scenario.both && p.ID != unavailable) {
					t.Fatal("stale configuration masqueraded as availability", p.ID, p.Availability)
				}
			}
			submitInteractive(t, client, view, "first", "hello")
			want := "codex login"
			if unavailable == claudeagent.ProviderID {
				want = "claude を起動して /login"
			}
			end := inspectInteractive(t, client, "stale-login", func(v host.View) bool { return hasFinalText(v, want) && v.Progress == nil })
			if multiReport(t, end).Selection.Provider != unavailable || len(cli.Calls(t)) != 0 {
				t.Fatal("unavailable provider caused inference or automatic switching")
			}
			if unavailable == "openai-codex" {
				watch, err := client.WatchPermissions(t.Context(), "stale-login", view.Generation)
				if err != nil {
					t.Fatal(err)
				}
				defer watch.Cancel()
				setGatewayHumanMode(t, watch, permission.FullAccess)
				_, err = modelExchange(t.Context(), client.Extension, modelRequest{Action: "child.start", ID: "stale-login", Generation: view.Generation, InputID: "no-auth-child", Template: "worker-low", Task: "inspect only"})
				if err == nil || !strings.Contains(err.Error(), "provider_unavailable") || !strings.Contains(err.Error(), "codex login") {
					t.Fatal("child missing actionable login error", err)
				}
			}
		})
	}
}

func TestProviderCombinationsCodexWithoutCLIIncludingDockerOAuth(t *testing.T) {
	for _, docker := range []bool{false, true} {
		t.Run(map[bool]string{false: "external", true: "docker"}[docker], func(t *testing.T) {
			_, cli := normalFixture(t, true, false)
			t.Setenv("PATH", cli.Directory)
			if err := os.Remove(filepath.Join(cli.Directory, "codex")); err != nil {
				t.Fatal(err)
			}
			if docker {
				path := sandboxCodexFixture(t)
				t.Setenv("CODEX_HOME", filepath.Dir(path))
				t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
				t.Setenv("HTTPS_PROXY", "http://host.docker.internal:3128")
			}
			o, _ := parseLaunchOptions([]string{"--bootstrap-codex-auto", "--bootstrap-claude-binary", cli.Binary}, io.Discard, os.Getenv)
			cfg, err := bootstrapRuntime(t.Context(), o, chooseInitialProvider(io.Discard), os.Getenv)
			if err != nil || cfg.Runtime.Provider.Provider != "openai-codex" || isDockerSandboxCodex(cfg.Runtime.Provider.Auth) != docker || codexCLIExists(os.Getenv) {
				t.Fatal("usable source required a Codex CLI", err)
			}
			client, _, _ := startInteractiveHost(t, cfg)
			if _, err := client.Open(t.Context(), host.Create, "no-codex-cli"); err != nil {
				t.Fatal(err)
			}
			out, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.providers", ID: "no-codex-cli", Refresh: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range out.Providers {
				if p.ID == "openai-codex" && p.Availability != "available" {
					t.Fatal("CLI absence hid a usable source", p.Reason)
				}
			}
		})
	}
}
