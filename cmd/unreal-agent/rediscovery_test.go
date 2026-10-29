//go:build linux || darwin

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestModelRefreshRegistersNewlyAuthenticatedProviderInRunningHost(t *testing.T) {
	f, cli := normalFixture(t, true, false)
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		before, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range before.Providers {
			if p.ID == claudeagent.ProviderID && p.Availability == "available" {
				t.Fatal("signed out Claude registered")
			}
		}
		cli.Set(t, testclaude.Config{Catalog: hostClaudeCatalog, AgentFinal: "こんにちは。"})
		catalog, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.catalog", ID: id, Provider: claudeagent.ProviderID, Refresh: true})
		if err != nil || catalog.Catalog == nil || !catalog.Catalog.Available {
			t.Fatal("login was not rediscovered", err)
		}
		after, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range after.Providers {
			if p.ID == claudeagent.ProviderID {
				found = p.Availability == "available" && p.Tools
			}
		}
		if !found {
			t.Fatal("new provider not registered with runtime")
		}
		v, err := c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		stopMultiSession(t, c, v)
		v, err = c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		model, err := initialCatalogModel(*catalog.Catalog, "")
		if err != nil {
			t.Fatal(err)
		}
		choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "new-login", Provider: claudeagent.ProviderID, Model: model.ID, Effort: model.DefaultEffort}
		_, err = modelExchange(ctx, c.Extension, modelRequest{Action: "model.select", ID: id, Generation: v.Generation, Selection: choice})
		if err != nil {
			t.Fatal("cannot switch stopped session", err)
		}
		resumed, err := c.Open(ctx, host.Resume, id)
		if err != nil {
			t.Fatal("cannot resume with new provider", err)
		}
		_, err = c.Submit(ctx, id, resumed.Generation, inbox.Input{ID: "greeting", Kind: inbox.InputExternal, Payload: []byte(`"こんにちは"`)})
		if err != nil {
			t.Fatal(err)
		}
		inspectInteractive(t, c, id, func(v host.View) bool {
			for _, item := range v.History.Items {
				if response, ok := item.Data.(sessionstore.ModelResponse); ok {
					for _, output := range response.Response.Output {
						if func() bool {
							message, ok := output.Data.(llm.Message)
							return ok && message.Text == "こんにちは。"
						}() {
							return true
						}
					}
				}
			}
			return false
		})
		if f.starts.Load() != 1 || len(cli.Calls(t)) != 1 {
			t.Fatal("restart or speculative inference during refresh")
		}
		return nil
	}}
	if err := l.run(t.Context(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableStartupGuidanceHasCommandsAndNoInternalCodes(t *testing.T) {
	for _, providers := range [][]modelcatalog.Provider{
		{unavailableProvider("openai-codex", modelcatalog.AuthRequired), unavailableProvider("claude", modelcatalog.AuthRequired)},
		{unavailableProvider("openai-codex", modelcatalog.CLIMissing), unavailableProvider("claude", modelcatalog.CLIMissing)},
	} {
		text := (&normalRuntimeError{Code: "providers_unavailable", Providers: providers}).Error()
		wants := []string{"unreal を起動", "Codex", "Claude"}
		if providers[0].Reason == modelcatalog.CLIMissing {
			wants = append(wants, "をインストールしてから", "Codex CLI", "Claude Code")
		} else {
			wants = append(wants, "codex login", "claude を起動して /login")
		}
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatal(want, text)
			}
		}
		for _, gone := range []string{"auth_required", "cli_missing", "openai-codex", "status="} {
			if strings.Contains(text, gone) {
				t.Fatal(gone, text)
			}
		}
	}
}

func TestModelRefreshAddsCodexAfterClaudeOnlyStartup(t *testing.T) {
	f, cli := normalFixture(t, false, true)
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		writeCodexAuth(t, filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"), "synthetic-later-token", "synthetic-account")
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.catalog", ID: id, Provider: "openai-codex", Refresh: true})
		if err != nil || out.Catalog == nil || !out.Catalog.Available {
			t.Fatal("Codex was not discovered", err)
		}
		v, err := c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		active := multiReport(t, v).Selection
		if active == nil || active.Provider != claudeagent.ProviderID {
			t.Fatal("refresh switched provider automatically")
		}
		model, err := initialCatalogModel(*out.Catalog, "")
		if err != nil {
			t.Fatal(err)
		}
		choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "codex-later", Provider: "openai-codex", Model: model.ID, Effort: model.DefaultEffort}
		if _, err = modelExchange(ctx, c.Extension, modelRequest{Action: "model.select", ID: id, Generation: v.Generation, Selection: choice}); err != nil {
			t.Fatal(err)
		}
		// A second discovery must keep bindings stable while the selection is pending.
		if _, err = modelExchange(ctx, c.Extension, modelRequest{Action: "model.catalog", ID: id, Refresh: true}); err != nil {
			t.Fatal(err)
		}
		if f.starts.Load() != 1 || len(cli.Calls(t)) != 0 {
			t.Fatal("refresh restarted or inferred")
		}
		return nil
	}}
	if err := l.run(t.Context(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
}
