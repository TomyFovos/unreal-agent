//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
	"time"
)

const hostClaudeCatalog = `[
 {"value":"claude-configured-a","resolvedModel":"wire-a","displayName":"Discovered A","supportsEffort":true,"supportedEffortLevels":["low","medium"]},
 {"value":"discovered-b","resolvedModel":"wire-b","displayName":"Discovered B","supportsEffort":true,"supportedEffortLevels":["high","xhigh","unknown"]},
 {"value":"no-effort","resolvedModel":"wire-no-effort","displayName":"No effort model","supportsEffort":false,"supportedEffortLevels":[]}
]`

func TestClaudeDiscoveredCatalogGatewaySelectionResumeAndAnalysis(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(map[bool]string{true: "explicit fallback", false: "discovery only"}[explicit], func(t *testing.T) {
			f := testclaude.New(t)
			f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog})
			config := claudeServeConfiguration(t, f)
			config.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
			if !explicit {
				config.Runtime.ClaudeCode.Models = nil
			}
			c, store, _ := startInteractiveHost(t, config)
			v, err := c.Open(t.Context(), host.Create, "discovered-catalog")
			if err != nil {
				t.Fatal(err)
			}
			fetch := func(refresh bool) modelReply {
				t.Helper()
				r, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "discovered-catalog", Refresh: refresh})
				if err != nil || r.Catalog == nil || !r.Catalog.Authoritative {
					t.Fatal("not a discovered catalog", err)
				}
				return r
			}
			catalog := fetch(false)
			if len(catalog.Catalog.Models) != 3 || catalog.Catalog.Models[1].ID != "discovered-b" || catalog.Catalog.Models[1].ResolvedModel != "wire-b" || len(catalog.Catalog.Models[1].Efforts) != 2 {
				t.Fatal("gateway lost model metadata")
			}
			fetch(false)
			if len(f.CatalogCalls(t)) != 1 || len(f.Calls(t)) != 0 {
				t.Fatal("picker lookup repeated discovery or performed inference")
			}
			submitInteractive(t, c, v, "first", "offline first")
			inspectInteractive(t, c, "discovered-catalog", func(v host.View) bool { return responseCount(v) == 1 })
			choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "discovered-choice", Model: "discovered-b", Effort: "xhigh"}
			if _, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "discovered-catalog", Generation: v.Generation, Selection: choice}); err != nil {
				t.Fatal(err)
			}
			// A canonical selected alias is distinct from the next response's wire ID.
			f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, Stream: `{"type":"system","subtype":"init","model":"wire-b","tools":[],"permissionMode":"default","mcp_servers":[],"plugins":[{"name":"catalog-private-sensitive","path":"/catalog-private-sensitive"}]}` + "\n" + `{"type":"assistant","message":{"id":"msg_wire","model":"wire-b","content":[{"type":"text","text":"offline response"}]}}` + "\n" + `{"type":"result","subtype":"success","usage":{"input_tokens":10,"output_tokens":4}}` + "\n"})
			submitInteractive(t, c, v, "second", "offline second")
			current := inspectInteractive(t, c, "discovered-catalog", func(v host.View) bool { return responseCount(v) == 2 || v.Failure != "" })
			if current.Failure != "" || len(current.Operations) != 0 {
				t.Fatal("discovered selection failed or executed tools", current.Failure)
			}
			calls := f.Calls(t)
			if len(calls) != 2 {
				t.Fatal("duplicate inference")
			}
			cliArgument(t, calls[1], "--model", "discovered-b")
			cliArgument(t, calls[1], "--effort", "xhigh")
			var revisions []uint64
			for _, item := range current.History.Items {
				if turn, ok := item.Data.(session.Turn); ok {
					revisions = append(revisions, turn.RuntimeRevision)
				}
			}
			if len(revisions) != 2 || revisions[0] != 0 || revisions[1] != 1 {
				t.Fatal("prior turns changed revision", revisions)
			}
			accumulator := analysis.New("discovered-catalog")
			accumulator.Created(current.Session.Session.CreatedAt)
			for _, item := range current.History.Items {
				accumulator.Apply(item)
			}
			report := accumulator.Snapshot(time.Now(), current.Running, false, nil, current.Operations)
			if report.Selection == nil || report.Selection.Model != "discovered-b" || report.Selection.Effort != "xhigh" || len(report.Turns) != 2 {
				t.Fatal("analysis lost canonical model revisions")
			}
			foundWire := false
			for _, item := range current.History.Items {
				if response, ok := item.Data.(sessionstore.ModelResponse); ok && response.Response.Model == "wire-b" {
					foundWire = true
				}
			}
			if !foundWire {
				t.Fatal("observed model identity was replaced by alias")
			}
			for _, format := range []string{"JSON", "Markdown"} {
				data, err := analysis.Encode(report, format)
				if err != nil || strings.Contains(string(data), "catalog-private-sensitive") || strings.Contains(string(data), "account-sensitive") {
					t.Fatal("catalog/account metadata leaked into analysis", err)
				}
			}
			noEffort := sessionstore.RuntimeSelection{Version: 1, RequestID: "no-effort-choice", Model: "no-effort", Effort: ""}
			if _, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "discovered-catalog", Generation: v.Generation, Expected: 1, Selection: noEffort}); err != nil {
				t.Fatal("model without effort unsupported", err)
			}
			f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog})
			submitInteractive(t, c, v, "third", "offline third")
			third := inspectInteractive(t, c, "discovered-catalog", func(v host.View) bool { return responseCount(v) == 3 || v.Failure != "" })
			if third.Failure != "" {
				t.Fatal(third.Failure)
			}
			for _, arg := range f.Calls(t)[2].Arguments {
				if arg == "--effort" {
					t.Fatal("invented effort for unsupported model")
				}
			}
			if _, err = c.Stop(t.Context(), "discovered-catalog", v.Generation, "when_idle", "test"); err != nil {
				t.Fatal(err)
			}
			inspectInteractive(t, c, "discovered-catalog", func(v host.View) bool { return !v.Running })
			c2, _, _ := startInteractiveHost(t, config, "--session-directory", store)
			resumed, err := c2.Open(t.Context(), host.Resume, "discovered-catalog")
			if err != nil {
				t.Fatal("discovered model resume failed", err)
			}
			if len(f.Calls(t)) != 3 {
				t.Fatal("resume sent speculative inference")
			}
			data, err := json.Marshal(resumed.History)
			if err != nil || strings.Contains(string(data), "wire-no-effort") || strings.Contains(string(data), "account-sensitive") {
				t.Fatal("discovery cache entered canonical history", err)
			}
			submitInteractive(t, c2, resumed, "fourth", "offline fourth")
			fourth := inspectInteractive(t, c2, "discovered-catalog", func(v host.View) bool { return responseCount(v) == 4 || v.Failure != "" })
			if fourth.Failure != "" || len(fourth.Operations) != 0 {
				t.Fatal("restored selection failed", fourth.Failure)
			}
			cliArgument(t, f.Calls(t)[3], "--model", "no-effort")
		})
	}
}

func TestClaudeCatalogFailureWithoutExplicitModelsKeepsCurrentOnly(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	config.Runtime.ClaudeCode.Models = nil
	c, _, _ := startInteractiveHost(t, config)
	v, err := c.Open(t.Context(), host.Create, "current-only")
	if err != nil {
		t.Fatal(err)
	}
	r, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "current-only"})
	if err != nil || r.Catalog == nil || r.Catalog.Available || r.Catalog.Authoritative || len(r.Catalog.Models) != 0 {
		t.Fatal("unavailable discovery invented a catalog", err)
	}
	submitInteractive(t, c, v, "input", "offline response")
	current := inspectInteractive(t, c, "current-only", func(v host.View) bool { return responseCount(v) == 1 || v.Failure != "" })
	if current.Failure != "" || len(current.Operations) != 0 {
		t.Fatal("current canonical model fallback failed", current.Failure)
	}
}

func TestClaudeRestrictedCatalogExcludesConfiguredAndCurrentModels(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	f.Set(t, testclaude.Config{Catalog: `[{"value":"restricted-only","displayName":"Allowed","supportsEffort":true,"supportedEffortLevels":["high"]}]`})
	c, _, _ := startInteractiveHost(t, config)
	v, err := c.Open(t.Context(), host.Create, "restricted")
	if err != nil {
		t.Fatal(err)
	}
	r, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "restricted"})
	if err != nil || r.Catalog == nil || len(r.Catalog.Models) != 1 || r.Catalog.Models[0].ID != "restricted-only" {
		t.Fatal("explicit models widened organization catalog", err)
	}
	for _, model := range []string{"claude-configured-a", "claude-configured-b"} {
		_, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "restricted", Generation: v.Generation, Selection: sessionstore.RuntimeSelection{Version: 1, RequestID: model, Model: model, Effort: "medium"}})
		if err == nil {
			t.Fatal("selected a model omitted by organization policy")
		}
	}
	if len(f.Calls(t)) != 0 {
		t.Fatal("catalog selection sent inference")
	}
}
