//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestCommonBootstrapUsesNormalDiscoveryAndExplicitTrust(t *testing.T) {
	for _, providers := range []struct {
		name                 string
		codex, claude, trust bool
		want                 int
	}{
		{"codex", true, false, false, 1}, {"claude", false, true, true, 1}, {"both", true, true, true, 2},
		// The Claude Code route (U21) runs under the organization's own managed
		// settings; the old route's managed-trust flags are not needed for it.
		{"no-auth", false, false, false, 0}, {"team-without-trust-flags", false, true, false, 1},
	} {
		t.Run(providers.name, func(t *testing.T) {
			_, cli := normalFixture(t, providers.codex, providers.claude)
			cli.Set(t, testclaude.Config{Subscription: "team", SignedOut: !providers.claude, Catalog: hostClaudeCatalog})
			args := []string{"--bootstrap-claude-binary", cli.Binary, "dev"}
			o, err := parseLaunchOptions(args, io.Discard, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := bootstrapRuntime(t.Context(), o, func(ids []string) (string, error) { return ids[0], nil }, os.Getenv)
			if providers.want == 0 {
				if err == nil {
					t.Fatal("unavailable providers claimed available")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Providers)+1 != providers.want || cfg.Launcher.DiscoverProviders != nil && !*cfg.Launcher.DiscoverProviders {
				t.Fatal("common startup retained single-provider Pilot")
			}
			if !reflect.DeepEqual(cfg.Permissions.Tools, []string{"read", "grep", "glob"}) || cfg.Permissions.FilesystemUnrestricted || cfg.Permissions.NetworkUnrestricted {
				t.Fatal("common package changed baseline grants")
			}
			if len(cli.Calls(t)) != 0 {
				t.Fatal("discovery sent inference")
			}
		})
	}
}

func TestCommonBootstrapDockerProxyAvailability(t *testing.T) {
	for _, proxy := range []string{"http://host.docker.internal:3128", "", "http://127.0.0.1:3128", "http://PRIVATE_USER:PRIVATE_VALUE@host.docker.internal:3128"} {
		t.Run(map[bool]string{true: "valid", false: "invalid"}[proxy == "http://host.docker.internal:3128"], func(t *testing.T) {
			_, cli := normalFixture(t, false, false)
			path := sandboxCodexFixture(t)
			t.Setenv("CODEX_HOME", filepath.Dir(path))
			t.Setenv("SBX_CRED_OPENAI_MODE", "oauth")
			t.Setenv("HTTPS_PROXY", proxy)
			t.Setenv("https_proxy", "")
			o, err := parseLaunchOptions([]string{"--bootstrap-codex-docker-oauth", "--bootstrap-claude-binary", cli.Binary}, io.Discard, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := bootstrapRuntime(t.Context(), o, nil, os.Getenv)
			if proxy == "http://host.docker.internal:3128" {
				if err != nil || cfg.Runtime.Provider.Auth.Method != credential.DockerSandboxOAuth || !cfg.Launcher.CodexDockerSandboxOAuth {
					t.Fatal("explicit OAuth route lost", err)
				}
			} else if err == nil {
				t.Fatal("bad proxy registered as available")
			}
			if len(cli.Calls(t)) != 0 {
				t.Fatal("discovery inferred")
			}
		})
	}
}

func TestCommonBootstrapFlagsDoNotApproveOrOverwriteExistingConfig(t *testing.T) {
	f, _ := normalFixture(t, true, false)
	for _, args := range [][]string{{"--bootstrap-claude-managed-trust"}, {"--bootstrap-claude-policy-audited"}} {
		if _, err := parseLaunchOptions(args, io.Discard, os.Getenv); err == nil {
			t.Fatal("one-sided trust accepted")
		}
	}
	f.writeConfig(t)
	before, err := os.ReadFile(f.o.config)
	if err != nil {
		t.Fatal(err)
	}
	o, err := parseLaunchOptions([]string{"--config", f.o.config, "--bootstrap-codex-docker-oauth", "dev"}, io.Discard, os.Getenv)
	if err != nil || string(o.id) != "dev" {
		t.Fatal("bool bootstrap flags consumed session argument", err)
	}
	if _, err = resolveLauncherConfig(t.Context(), o, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.o.config)
	if !bytes.Equal(before, after) {
		t.Fatal("first-run opt-in overwrote existing runtime")
	}
}

func TestLauncherIdentityRequiresExactBuildAndProtocol(t *testing.T) {
	binding := launcherHostBinding{Configuration: "/synthetic/runtime.json", Identity: ownLauncherIdentity()}
	if binding.Identity.Build == "" {
		t.Fatal("own build unverifiable")
	}
	h := modelUI{binding: &binding}
	for _, kind := range []string{"same", "old-no-identity", "other-build", "other-protocol", "same-path-only"} {
		t.Run(kind, func(t *testing.T) {
			copy := binding
			switch kind {
			case "old-no-identity", "same-path-only":
				copy.Identity = launcherIdentity{}
			case "other-build":
				copy.Identity.Build = "different"
			case "other-protocol":
				copy.Identity.Protocol++
			}
			raw, _ := json.Marshal(modelRequest{Action: "launcher.check", HostBinding: &copy})
			r := httptest.NewRequest(http.MethodPost, "http://localhost/control", bytes.NewReader(raw))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var reply modelReply
			if json.Unmarshal(w.Body.Bytes(), &reply) != nil {
				t.Fatal("invalid safe reply")
			}
			if (reply.Error == "") != (kind == "same") {
				t.Fatal("old Host accepted solely by path equality")
			}
		})
	}
}
