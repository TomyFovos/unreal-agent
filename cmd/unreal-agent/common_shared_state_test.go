//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/authcli"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestCommonLauncherSharedStoreAcrossWorkspacesAndInstances(t *testing.T) {
	scripts, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	models := openCodeGoTestList(t)
	for _, xdg := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-state", true: "xdg-state"}[xdg], func(t *testing.T) {
			f, cli := normalFixture(t, true, false)
			t.Setenv("UA_TEST_OPENCODE_GO_MODELS", models)
			home := os.Getenv("HOME")
			stateHome := filepath.Join(home, ".local", "state")
			if xdg {
				stateHome = filepath.Join(home, "custom state")
				t.Setenv("XDG_STATE_HOME", stateHome)
			}
			shared := filepath.Join(stateHome, "unreal-agent")
			store := filepath.Join(shared, "credentials")
			// Register through the same auth entry point and path as the common
			// installer; no real credentials, model calls or external API are used.
			const key = "synthetic-common-go-key"
			if err := authcli.Run(t.Context(), []string{"login", "--store", store, "--provider", "opencode-go", "--id", "primary", "--key-stdin"}, strings.NewReader(key), io.Discard); err != nil {
				t.Fatal(err)
			}
			login := openCodeGoKeyCommand(os.Getenv)
			loginStore := store
			if !xdg {
				loginStore = "~/.local/state/unreal-agent/credentials"
			}
			if !strings.Contains(login, "--store "+shellWord(loginStore)+" --provider opencode-go") {
				t.Fatal("/login and the installer disagree about the store")
			}
			states := map[string]bool{}
			legacy := ""
			legacyChoice := []byte(`{"Version":2,"Roles":{"worker":{"Provider":"opencode-go","Model":"deepseek-v4.1-flash","Effort":"low"}}}`)
			for index, target := range []struct{ workspace, instance string }{{"first", "default"}, {"second", "default"}, {"first", "x"}, {"first", "default"}} {
				workspace := filepath.Join(home, target.workspace)
				if err := os.MkdirAll(workspace, 0700); err != nil {
					t.Fatal(err)
				}
				// Exercise the real Python command builder, then start its Host
				// with the real Go launcher and serve composition.
				python := exec.CommandContext(t.Context(), "python3", "-B", "-c", `
import json, sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import common_development as common
home, workspace = Path(sys.argv[2]), Path(sys.argv[3])
settings = {"Home": str(home), "BinaryDirectory": "/unused", "DockerOAuth": False, "ClaudeBinary": sys.argv[5]}
paths = common.repo_paths(home, workspace, sys.argv[4])
print(json.dumps({"args": common.command_for(settings, workspace, paths, "dev")[1:],
                 "config_home": str(paths["xdg"]), "state": str(paths["state"])}))
`, scripts, home, workspace, target.instance, cli.Binary)
				raw, err := python.CombinedOutput()
				if err != nil {
					t.Fatalf("common launcher arguments: %v: %s", err, raw)
				}
				var launch struct {
					Args       []string `json:"args"`
					ConfigHome string   `json:"config_home"`
					State      string   `json:"state"`
				}
				if err := json.Unmarshal(raw, &launch); err != nil {
					t.Fatal(err)
				}
				t.Setenv("XDG_CONFIG_HOME", launch.ConfigHome)
				states[launch.State] = true
				if index == 0 {
					legacy = filepath.Join(launch.State, "subagent-model.json")
					if err := os.MkdirAll(launch.State, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(legacy, legacyChoice, 0600); err != nil {
						t.Fatal(err)
					}
				}
				launcher := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
					providers, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id, Refresh: true})
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, p := range providers.Providers {
						found = found || p.ID == opencodego.ProviderID && p.Availability == "available"
					}
					if !found {
						t.Fatal("launcher Host cannot find the installer key")
					}
					var output bytes.Buffer
					command := subagentCLI{client: c, parent: id, out: &output, name: "unreal-agent"}
					if err := command.model(ctx, []string{"show", "--role", "worker"}); err != nil {
						t.Fatal(err)
					}
					wantEffort := "low"
					if index >= 2 {
						wantEffort = "high"
					}
					if !strings.Contains(output.String(), "worker: provider=opencode-go model=deepseek-v4.1-flash effort="+wantEffort) {
						t.Fatalf("role choice in %s/%s = %s; want effort %s", target.workspace, target.instance, output.String(), wantEffort)
					}
					if index == 0 {
						return command.model(ctx, []string{"set", "--role", "worker", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "max"})
					}
					if index == 1 {
						if data, err := os.ReadFile(filepath.Join(shared, "subagent-model.json")); err != nil || !bytes.Equal(data, legacyChoice) {
							t.Fatal("a Session change modified the migrated default", err)
						}
						return command.model(ctx, []string{"set", "--all-sessions", "--role", "worker", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "high"})
					}
					return nil
				}}
				if err := launcher.run(t.Context(), launch.Args, io.Discard); err != nil {
					t.Fatal(err)
				}
				log, err := os.ReadFile(filepath.Join(launch.State, "host.log"))
				if err != nil {
					t.Fatal(err)
				}
				assertNoCredentialLeak(t, string(log), key)
			}
			if len(states) != 3 || f.starts.Load() != 3 || f.calls.Load() != 0 || len(cli.Calls(t)) != 0 {
				t.Fatal("workspace/instance isolation or discovery without inference lost")
			}
			if data, err := os.ReadFile(legacy); err != nil || !bytes.Equal(data, legacyChoice) {
				t.Fatal("migration changed the original per-repo choice", err)
			}
			choice, err := readSubagentModel(filepath.Join(shared, "subagent-model.json"))
			if err != nil || choice.choice("worker").Effort != "high" {
				t.Fatal("explicit all-Sessions update was not saved as a default", err)
			}
		})
	}
}
