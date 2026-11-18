//go:build linux || darwin

package main

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestSubagentModelCLIShowSetEachRoleAndAll(t *testing.T) {
	t.Parallel()
	h := startOpenCodeGoChildren(t, true, false)
	h.designer(t)
	// An old single selection applies to every role, without migration on read.
	legacy := []byte(`{"Version":1,"Provider":"opencode-go","Model":"deepseek-v4.1-flash"}`)
	if err := os.WriteFile(h.modelFile, legacy, 0600); err != nil {
		t.Fatal(err)
	}
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
	saved, err := readSubagentModel(h.modelFile)
	if err != nil || saved.Version != 2 {
		t.Fatalf("role format: %+v, %v", saved, err)
	}
	for _, role := range subagentRoleGroups {
		if saved.choice(role) != (subagentModelChoice{Provider: "opencode-go", Model: "deepseek-v4.1-flash", Effort: llm.ReasoningEffortMax}) {
			t.Fatalf("saved %s: %+v", role, saved.choice(role))
		}
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
	before, err := os.ReadFile(h.modelFile)
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
		after, err := os.ReadFile(h.modelFile)
		if err != nil || !slices.Equal(before, after) {
			t.Fatalf("failed set changed saved choice: %s, %v", after, err)
		}
	}
	// The Host must validate catalog membership even when a caller bypasses
	// the CLI's required model flag; an empty Codex choice is no longer a setter shortcut.
	if _, err := h.choose(t, "openai-codex", ""); err == nil || !strings.Contains(err.Error(), "invalid_model") {
		t.Fatalf("Host accepted a model outside the catalog: %v", err)
	}
	if after, err := os.ReadFile(h.modelFile); err != nil || !bytes.Equal(before, after) {
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
	if choices, err := readSubagentModel(h.modelFile); err != nil || !choices.choice("worker").Fast || choices.choice("lead").Fast {
		t.Fatalf("saved Fast choice escaped its role: %+v, %v", choices, err)
	}
	h.start(t)
	h.designer(t)
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
