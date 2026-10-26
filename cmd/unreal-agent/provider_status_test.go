//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

type noProviderInference struct{ t *testing.T }

func (m noProviderInference) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	m.t.Error("provider metadata request caused inference")
	return llm.Response{}, errors.New("synthetic unexpected inference")
}

func TestCommonCatalogRefreshRequiresRegisteredAuthenticatedBackend(t *testing.T) {
	owner, err := host.New(t.Context(), host.Config{Directory: privateCLIDirectory(t), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: noProviderInference{t}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	s, err := owner.Create(t.Context(), host.Options{ID: "catalog-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	var reads, refreshes int
	var signedOut bool
	h := modelUI{owner: owner, supported: []modelcatalog.Provider{{ID: "openai-codex"}, unavailableProvider("claude-code", modelcatalog.AuthRequired)}, cache: "synthetic-cache",
		runtime: agentrunner.RuntimeConfig{Identity: agentrunner.RuntimeIdentity{Provider: provider.Selection{Provider: "openai-codex"}},
			Health: map[string]func(context.Context) error{"openai-codex": func(context.Context) error {
				if signedOut {
					return errors.New("synthetic closed health failure")
				}
				return nil
			}}}, codexRead: func(_ context.Context, refresh bool) (modelcatalog.Catalog, error) {
			reads++
			if refresh {
				refreshes++
				return modelcatalog.Catalog{Available: true}, nil
			}
			return modelcatalog.Catalog{Problem: "catalog unavailable"}, nil
		}}
	for _, tc := range []struct {
		id, want           string
		refresh, signedOut bool
	}{
		{"openai-codex", "provider_unavailable", false, false},
		{"openai-codex", "", true, false},
		{"openai-codex", "provider_unavailable", true, true},
		{"claude-code", "provider_unavailable", true, false},
	} {
		signedOut = tc.signedOut
		raw, _ := json.Marshal(modelRequest{Action: "model.catalog", ID: s.ID, Provider: tc.id, Refresh: tc.refresh})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://localhost/control", bytes.NewReader(raw)))
		var reply modelReply
		if json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Error != tc.want {
			t.Fatal("incorrect catalog refresh authorization", reply.Error, tc.want)
		}
		if tc.want == "" && (reply.Catalog == nil || !reply.Catalog.Available) {
			t.Fatal("metadata refresh blocked by stale cached availability")
		}
	}
	if reads != 2 || refreshes != 1 {
		t.Fatal("unregistered/unauthenticated refresh performed provider discovery")
	}
	view, err := s.Inspect(0, 32)
	if err != nil || len(view.Operations) != 0 || responseCount(view) != 0 {
		t.Fatal("catalog refresh caused execution or response")
	}
}

func TestCommonProviderStatusPreflightAndUnavailableSelection(t *testing.T) {
	for _, tc := range []struct {
		name                                                     string
		codex, claude, trust, missingCLI, missingCatalog, docker bool
		codexReason, claudeReason                                modelcatalog.AvailabilityReason
	}{
		{name: "both", codex: true, claude: true, trust: true},
		{name: "codex-only", codex: true, claudeReason: modelcatalog.AuthRequired},
		{name: "claude-only", claude: true, trust: true, codexReason: modelcatalog.AuthRequired},
		{name: "none", codexReason: modelcatalog.AuthRequired, claudeReason: modelcatalog.AuthRequired},
		// The Claude Code route (U21) runs under the organization's own managed
		// settings; it needs no managed-trust flags.
		{name: "team-without-trust", codex: true, claude: true},
		{name: "cli-missing", codex: true, missingCLI: true, claudeReason: modelcatalog.CLIMissing},
		{name: "catalog-missing", codex: true, missingCatalog: true, codexReason: modelcatalog.CatalogUnavailable, claudeReason: modelcatalog.AuthRequired},
		{name: "invalid-docker", docker: true, claude: true, trust: true, codexReason: modelcatalog.ProxyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cli := normalFixture(t, tc.codex, tc.claude)
			cli.Set(t, testclaude.Config{Subscription: "team", SignedOut: !tc.claude, Catalog: hostClaudeCatalog})
			binary := cli.Binary
			if tc.missingCLI {
				binary = filepath.Join(cli.Directory, "absent-cli")
			}
			if tc.missingCatalog {
				if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".codex/models_cache.json")); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--claude-binary", binary}
			if tc.docker {
				args = append(args, "--docker-sandbox-oauth")
			}
			var out bytes.Buffer
			if err := run(t.Context(), append([]string{"providers"}, args...), &out); err != nil {
				t.Fatal(err)
			}
			var reply struct{ Providers []modelcatalog.Provider }
			if json.Unmarshal(out.Bytes(), &reply) != nil || len(reply.Providers) != 2 {
				t.Fatal("missing safe statuses")
			}
			for _, p := range reply.Providers {
				want := tc.codexReason
				if p.ID == claudeagent.ProviderID {
					want = tc.claudeReason
				}
				if p.Reason != want || (p.Availability == "available") != (want == "") || len(p.Catalog.Models) != 0 || p.Catalog.Source != "" {
					t.Fatal("unsafe or incorrect preflight", p.ID, p.Reason, want)
				}
			}
			if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), os.Getenv("HOME")) || len(cli.Calls(t)) != 0 {
				t.Fatal("preflight leaked data or performed inference")
			}
			o, err := parseLaunchOptions([]string{"--bootstrap-claude-binary", binary}, io.Discard, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			o.bootstrapDocker = tc.docker
			cfg, err := bootstrapRuntime(t.Context(), o, func([]string) (string, error) { return "openai-codex", nil }, os.Getenv)
			available := tc.codexReason == "" || tc.claudeReason == ""
			if !available {
				var failure *normalRuntimeError
				if !errors.As(err, &failure) || !strings.Contains(failure.Error(), "openai-codex:") || !strings.Contains(failure.Error(), claudeagent.ProviderID+":") {
					t.Fatal("startup lost both statuses", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			client, _, _ := startInteractiveHost(t, cfg)
			view, err := client.Open(t.Context(), host.Create, "provider-status")
			if err != nil {
				t.Fatal(err)
			}
			providers, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.providers", ID: "provider-status"})
			if err != nil || len(providers.Providers) != 2 {
				t.Fatal("TUI missing supported providers", err)
			}
			for _, p := range providers.Providers {
				if p.Availability == "available" {
					continue
				}
				_, err = modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.select", ID: "provider-status", Generation: view.Generation, Selection: sessionstore.RuntimeSelection{Version: 1, Provider: p.ID, Model: "not-a-real-model", Effort: "high"}})
				if err == nil || err.Error() != "provider_unavailable" {
					t.Fatal("unavailable provider selected", err)
				}
			}
			after, err := client.Inspect(t.Context(), "provider-status", 0, 256)
			if err != nil || len(after.Operations) != 0 || len(cli.Calls(t)) != 0 || multiReport(t, after).Selection.Provider != cfg.Runtime.Provider.Provider {
				t.Fatal("selection caused execution/fallback")
			}

		})
	}
}

func TestCommonPreflightExplicitDeclinesAndTrustPair(t *testing.T) {
	_, cli := normalFixture(t, true, true)
	for _, args := range [][]string{{"--claude-managed-trust"}, {"--claude-policy-audited"}, {"--docker-sandbox-oauth", "--codex-disabled"}, {"--claude-disabled", "--claude-managed-trust", "--claude-policy-audited"}} {
		if runProviderPreflight(t.Context(), args, io.Discard) == nil {
			t.Fatal("invalid confirmation accepted")
		}
	}
	var out bytes.Buffer
	if err := runProviderPreflight(t.Context(), []string{"--codex-disabled", "--claude-disabled"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "operator_disabled") != 2 || len(cli.Calls(t)) != 0 {
		t.Fatal("declines not retained")
	}
}

func TestCommonProviderPreflightExecutableStreams(t *testing.T) {
	args := []string{"providers", "--codex-disabled", "--claude-disabled"}
	var stdout, stderr bytes.Buffer
	if err := runExecutableWithStreams(t.Context(), "unreal-agent", args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	assertProviderStdout := func(t *testing.T, out, diagnostics []byte) {
		t.Helper()
		var reply struct{ Providers []modelcatalog.Provider }
		if json.Unmarshal(out, &reply) != nil || len(reply.Providers) != 2 || len(diagnostics) != 0 {
			t.Fatal("machine status must be JSON on stdout only")
		}
		for _, p := range reply.Providers {
			if p.Availability != "unavailable" || p.Reason != modelcatalog.ProviderDisabled {
				t.Fatal("explicit declines were not retained")
			}
		}
	}
	assertProviderStdout(t, stdout.Bytes(), stderr.Bytes())
	for _, tc := range []struct {
		name string
		args []string
	}{{"unreal", []string{"--help"}}, {"unreal-agent", []string{"serve", "--help"}}} {
		stdout.Reset()
		stderr.Reset()
		_ = runExecutableWithStreams(t.Context(), tc.name, tc.args, &stdout, &stderr)
		if stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatal("human launcher/help diagnostics changed streams")
		}
	}
	if directory := os.Getenv("UNREAL_COMMON_BIN_DIR"); directory != "" {
		stdout.Reset()
		stderr.Reset()
		cmd := exec.CommandContext(t.Context(), filepath.Join(directory, "unreal-agent"), args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatal("packaged metadata-only preflight failed", err)
		}
		assertProviderStdout(t, stdout.Bytes(), stderr.Bytes())
	}
}

// A new instance starts with Claude when it is available, without a question;
// /model switches (U8, U25).
func TestInitialProviderIsChosenWithoutAsking(t *testing.T) {
	var out strings.Builder
	choose := chooseInitialProvider(&out)
	if id, err := choose([]string{claudeagent.ProviderID, "openai-codex"}); err != nil || id != claudeagent.ProviderID {
		t.Fatalf("got %q, %v; want Claude", id, err)
	}
	if !strings.Contains(out.String(), "Starting with Claude") || strings.Contains(out.String(), "number") {
		t.Fatalf("printed %q", out.String())
	}
	if id, err := choose([]string{"openai-codex"}); err != nil || id != "openai-codex" {
		t.Fatalf("got %q, %v; want the only provider", id, err)
	}
}
