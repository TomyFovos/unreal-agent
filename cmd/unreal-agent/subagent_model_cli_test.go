//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestSubagentModelCLIShowSetEachRoleAndAll(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	// An old single selection seeds every role.
	legacy := []byte(`{"Version":1,"Provider":"opencode-go","Model":"deepseek-v4.1-flash"}`)
	if err := os.WriteFile(h.modelFile, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	h.designer(t)
	shown := h.mustCLI(t, "", "model", "show")
	for _, role := range subagentRoleGroups {
		if !strings.Contains(shown, role+": provider=opencode-go model=deepseek-v4.1-flash effort=template default fast=off") {
			t.Fatalf("legacy %s missing: %s", role, shown)
		}
	}
	if after, err := os.ReadFile(h.modelFile); err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("reading migrated the legacy file: %s, %v", after, err)
	}
	for _, choice := range []struct{ role, provider, model, effort string }{
		{"lead", "openai-codex", "gpt-6.2-sol", "max"},
		{"worker", "opencode-go", "deepseek-v4.1-flash", "max"},
		{"collector", "opencode-go", "deepseek-v4.1-flash", "low"},
	} {
		output := h.mustCLI(t, "", "model", "set", "--role", choice.role, "--provider", choice.provider, "--model", choice.model, "--effort", choice.effort)
		expected := choice.role + ": provider=" + choice.provider + " model=" + choice.model + " effort=" + choice.effort + " fast=off"
		if !strings.Contains(output, expected) || !strings.Contains(output, "next child start") {
			t.Fatalf("set %s: %s", choice.role, output)
		}
		if shown := h.mustCLI(t, "", "model", "show", "--role", choice.role); strings.TrimSpace(shown) != expected {
			t.Fatalf("show %s: %s", choice.role, shown)
		}
	}
	h.mustCLI(t, "", "model", "set", "--role", "all", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "max")
	for _, role := range subagentRoleGroups {
		if shown := h.mustCLI(t, "", "model", "show", "--role", role); strings.TrimSpace(shown) != role+": provider=opencode-go model=deepseek-v4.1-flash effort=max fast=off" {
			t.Fatalf("all did not replace %s: %s", role, shown)
		}
	}
	saved, err := readSubagentModel(h.roleFile("designer"))
	if err != nil || saved.Version != 2 {
		t.Fatalf("role format: %+v, %v", saved, err)
	}
	for _, role := range subagentRoleGroups {
		if saved.choice(role) != (subagentModelChoice{Provider: "opencode-go", Model: "deepseek-v4.1-flash", Effort: llm.ReasoningEffortMax}) {
			t.Fatalf("saved %s: %+v", role, saved.choice(role))
		}
	}
	if after, err := os.ReadFile(h.modelFile); err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("Session set changed the shared default: %s, %v", after, err)
	}
	// Each operation is recorded by the owning Session with its effective role choice.
	view, err := h.client.Inspect(t.Context(), "designer", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	setRoles, showRoles := map[string]bool{}, map[string]bool{}
	for _, item := range view.History.Items {
		if item.Kind != sessionstore.ItemHostRecord {
			continue
		}
		record, ok := item.Data.(sessionstore.HostRecord)
		if !ok {
			continue
		}
		activity := record.Activity
		if activity == nil || activity.Provider != "opencode-go" || activity.Model != "deepseek-v4.1-flash" || activity.Effort != "max" {
			continue
		}
		if activity.Kind == "subagent_model_set" {
			setRoles[activity.Template] = true
		}
		if activity.Kind == "subagent_model_show" {
			showRoles[activity.Template] = true
		}
	}
	for _, role := range subagentRoleGroups {
		if !setRoles[role] || !showRoles[role] {
			t.Fatalf("missing Session record for %s: set=%v show=%v", role, setRoles, showRoles)
		}
	}
	if h.codexCalls.Load() != 0 {
		t.Fatal("settings performed Codex inference")
	}
	h.server.mu.Lock()
	defer h.server.mu.Unlock()
	if len(h.server.models) != 0 {
		t.Fatal("settings performed Go inference")
	}
}

func TestSubagentModelCLIValidationPreservesChoice(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	h.mustCLI(t, "", "model", "set", "--role", "all", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "max")
	before, err := os.ReadFile(h.roleFile("designer"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--role", "worker", "--provider", "missing", "--model", "deepseek-v4.1-flash"}, "provider_unavailable: missing"},
		{[]string{"--role", "worker", "--provider", "opencode-go", "--model", "not-in-catalog"}, "invalid_model"},
		{[]string{"--role", "worker", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "medium"}, "unsupported_effort"},
		{[]string{"--role", "lead", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--effort", "ultra"}, "invalid effort"},
		{[]string{"--role", "worker", "--provider", "openai-codex", "--model", "gpt-6.2-sol", "--fast", "on"}, "fast unsupported for gpt-6.2-sol"},
		{[]string{"--role", "worker", "--provider", "opencode-go", "--model", "deepseek-v4.1-flash", "--fast", "on"}, "fast unsupported for deepseek-v4.1-flash"},
		{[]string{"--role", "all", "--fast", "on"}, "fast unsupported for deepseek-v4.1-flash"},
		{[]string{"--role", "worker", "--provider", "openai-codex", "--model", "gpt-6-luna", "--fast", "yes"}, "invalid fast mode"},
		{[]string{"--role", "unknown", "--provider", "openai-codex", "--model", "gpt-6.2-sol"}, "usage:"},
		{[]string{"--role", "worker", "--provider", "opencode-go"}, "requires --provider and --model"},
	} {
		output, err := h.cli(t, "", append([]string{"model", "set"}, tc.args...)...)
		if err == nil || !strings.Contains(output, tc.want) {
			t.Fatalf("%v: %s, %v; want %s", tc.args, output, err, tc.want)
		}
		after, err := os.ReadFile(h.roleFile("designer"))
		if err != nil || !slices.Equal(before, after) {
			t.Fatalf("failed set changed saved choice: %s, %v", after, err)
		}
	}
	// The Host must validate catalog membership even when a caller bypasses
	// the CLI's required model flag; an empty Codex choice is no longer a setter shortcut.
	if _, err := h.choose(t, "openai-codex", ""); err == nil || !strings.Contains(err.Error(), "invalid_model") {
		t.Fatalf("Host accepted a model outside the catalog: %v", err)
	}
	if after, err := os.ReadFile(h.roleFile("designer")); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("invalid Host selection changed settings: %s, %v", after, err)
	}

}

func TestSubagentModelCLIFastRoundTrip(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false, true)
	h.gpt.mu.Lock()
	h.gpt.routingOnly = true
	h.gpt.mu.Unlock()
	h.designer(t)
	set := func(fast string, model ...string) {
		t.Helper()
		args := []string{"model", "set", "--role", "worker", "--fast", fast}
		out := h.mustCLI(t, "", append(args, model...)...)
		if !strings.Contains(out, "worker: provider=openai-codex model=gpt-6-luna effort=max fast="+fast) {
			t.Fatalf("set %s: %s", fast, out)
		}
	}
	show := func(fast string) {
		t.Helper()
		out := h.mustCLI(t, "", "model", "show", "--role", "worker")
		if strings.TrimSpace(out) != "worker: provider=openai-codex model=gpt-6-luna effort=max fast="+fast {
			t.Fatalf("show %s: %s", fast, out)
		}
	}
	runWorker := func() {
		t.Helper()
		started := h.mustCLI(t, "Check saved worker routing.", "start", "--template", "lead")
		handle := childHandle.FindStringSubmatch(started)
		if handle == nil {
			t.Fatal(started)
		}
		h.waitReport(t, handle[1], "lead done")
	}
	set("on", "--provider", "openai-codex", "--model", "gpt-6-luna", "--effort", "max")
	if choices, err := readSubagentModel(h.roleFile("designer")); err != nil || !choices.choice("worker").Fast || choices.choice("lead").Fast {
		t.Fatalf("saved Fast choice escaped its role: %+v, %v", choices, err)
	}
	h.start(t)
	h.openSession(t, host.Resume, "designer")
	show("on")
	runWorker()
	set("off")
	show("off")
	runWorker()
	h.gpt.mu.Lock()
	defer h.gpt.mu.Unlock()
	if !slices.Equal(h.gpt.workerEffort, []string{"max", "max"}) ||
		!slices.Equal(h.gpt.workerModels, []string{"gpt-6-luna", "gpt-6-luna"}) ||
		!slices.Equal(h.gpt.workerTiers, []string{"priority", ""}) {
		t.Fatalf("worker requests: models=%v effort=%v tiers=%v", h.gpt.workerModels, h.gpt.workerEffort, h.gpt.workerTiers)
	}
}

func TestSubagentRoleSettingsIsolateSessionsAndNestedStarts(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false, true)
	h.claude.Set(t, testclaude.Config{Catalog: claudeChildCatalog})
	legacy := []byte(`{"Version":1,"Provider":"openai-codex"}`)
	if err := os.WriteFile(h.modelFile, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	h.designer(t)
	h.openSession(t, host.Create, "jh-ci")
	other := &openCodeGoChildren{
		gptHost: &gptHost{binary: h.binary, environment: h.environment},
		cfg:     h.cfg, modelFile: h.modelFile, store: h.store, codexAuth: h.codexAuth,
	}
	other.start(t)
	other.designer(t)

	hold := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(hold) })
	h.gpt.mu.Lock()
	h.gpt.routingOnly, h.gpt.leadHold = true, hold
	h.gpt.mu.Unlock()
	startLead := func(id session.ID) operation.ID {
		t.Helper()
		out := h.mustCLIFor(t, id, "Check Session worker selection.", "start", "--template", "lead")
		handle := childHandle.FindStringSubmatch(out)
		if handle == nil {
			t.Fatal(out)
		}
		return operation.ID(handle[1])
	}
	checkWorker := func(id session.ID, handle operation.ID, provider, model, effort string) {
		t.Helper()
		h.waitReportFor(t, id, string(handle), "lead done")
		dir := filepath.Join(h.directory, "sessions")
		lead, err := subagent.ReadChildSnapshot(t.Context(), dir, subagent.ChildID(id, handle))
		if err != nil {
			t.Fatal(err)
		}
		starts := 0
		for _, op := range lead.View.Operations {
			if op.ToolName != "SubagentStart" {
				continue
			}
			starts++
			plan, err := subagent.DecodePlan(op)
			if err != nil || op.Status != operation.StatusCompleted {
				t.Fatalf("worker did not complete: %+v, %v", op, err)
			}
			got := sessionstore.SelectionFromConfiguration(plan.Configuration.Runtime)
			if got == nil || got.Provider != provider || got.Model != model || string(got.Effort) != effort {
				t.Fatalf("%s worker = %+v; want %s/%s %s", id, got, provider, model, effort)
			}
		}
		if starts != 1 {
			t.Fatalf("%s started %d workers; want 1", id, starts)
		}
	}
	checkChoice := func(h *openCodeGoChildren, id session.ID, role, want string) {
		t.Helper()
		if got := strings.TrimSpace(h.mustCLIFor(t, id, "", "model", "show", "--role", role)); got != role+": "+want {
			t.Fatalf("%s in %s: %q", id, h.directory, got)
		}
	}
	original := "provider=openai-codex model=template default effort=template default fast=off"
	lead := startLead("jh-ci")
	waitInteractiveContext(t, func(context.Context) bool { return h.codexCalls.Load() > 0 })
	h.mustCLI(t, "", "model", "set", "--role", "worker", "--provider", opencodego.ProviderID, "--model", "deepseek-v4.1-flash", "--effort", "max")
	checkChoice(h, "jh-ci", "worker", original)
	checkChoice(other, "designer", "worker", original)
	if data, err := os.ReadFile(h.modelFile); err != nil || !bytes.Equal(data, legacy) {
		t.Fatalf("Session set wrote the shared default: %s, %v", data, err)
	}
	release.Do(func() { close(hold) })
	checkWorker("jh-ci", lead, "openai-codex", leadModel, "high")
	checkWorker("designer", startLead("designer"), opencodego.ProviderID, "deepseek-v4.1-flash", "max")
	h.server.check(t)

	h.start(t)
	h.openSession(t, host.Resume, "designer")
	h.openSession(t, host.Resume, "jh-ci")
	goWorker := "provider=opencode-go model=deepseek-v4.1-flash effort=max fast=off"
	checkChoice(h, "designer", "worker", goWorker)
	checkChoice(h, "jh-ci", "worker", original)
	h.mustCLI(t, "", "model", "set", "--role", "collector", "--provider", opencodego.ProviderID, "--model", "deepseek-v4.1-flash", "--effort", "max")
	output := h.mustCLI(t, "", "model", "set", "--all-sessions", "--role", "worker", "--provider", claudeagent.ProviderID, "--model", "claude-sonnet-5-5", "--effort", "low")
	if !strings.Contains(output, "Saved for all Sessions") {
		t.Fatal(output)
	}
	claudeWorker := "provider=claude model=claude-sonnet-5-5 effort=low fast=off"
	checkChoice(h, "designer", "worker", claudeWorker)
	checkChoice(h, "jh-ci", "worker", claudeWorker)
	checkChoice(other, "designer", "worker", claudeWorker)
	checkChoice(h, "designer", "collector", goWorker)
	checkChoice(h, "jh-ci", "collector", original)
	h.openSession(t, host.Create, "next")
	checkChoice(h, "next", "worker", claudeWorker)
	checkChoice(h, "next", "collector", original)
}
