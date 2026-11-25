package contextengine

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextPolicyResolvesProviderOverrides(t *testing.T) {
	var config Config
	raw := []byte(`{"RecentEventCount":8,"Routes":{
		"claude":{"WindowThresholdTokens":16001,"PostRebuildRatio":0.6,"RecentEventCount":0,"InputBudget":10001},
		"opencode-go":{"L2TargetTokens":3500},"openai-codex":{"WindowThresholdTokens":180000}}}`)
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	common := config.ForProvider("unconfigured")
	if common.WindowThresholdTokens != 200_000 || common.PostRebuildRatio != 0.5 || common.L2TargetTokens != 5000 ||
		common.RecentEventCount != 8 || common.RebuildTarget(99_999) != 49_999 {
		t.Fatalf("common policy = %+v", common)
	}
	claude := config.ForProvider("claude")
	if claude.WindowThresholdTokens != 16_001 || claude.PostRebuildRatio != 0.6 || claude.L2TargetTokens != 5000 ||
		claude.RecentEventCount != 0 || claude.InputBudget != 10_001 || claude.RebuildTarget(claude.InputBudget) != 6_000 {
		t.Fatalf("Claude policy = %+v", claude)
	}
	policies := config.Policies()
	if policies.ForProvider("claude") != claude.windowPolicy() || policies.ForProvider("opencode-go").L2TargetTokens != 3500 ||
		policies.ForProvider("openai-codex").WindowThresholdTokens != 180_000 || policies.ForProvider("other") != common.windowPolicy() {
		t.Fatalf("recorded policies = %+v", policies)
	}
}

func TestContextPolicyValidation(t *testing.T) {
	for _, test := range []struct{ raw, field string }{
		{`{"WindowThresholdTokens":0}`, "WindowThresholdTokens"},
		{`{"WindowThresholdTokens":-1}`, "WindowThresholdTokens"},
		{`{"PostRebuildRatio":0}`, "PostRebuildRatio"},
		{`{"PostRebuildRatio":-0.1}`, "PostRebuildRatio"},
		{`{"PostRebuildRatio":1}`, "PostRebuildRatio"},
		{`{"L2TargetTokens":100001}`, "L2TargetTokens"},
		{`{"Routes":{"claude":{"WindowThresholdTokens":0}}}`, `Routes["claude"]: WindowThresholdTokens`},
		{`{"Routes":{"claude":{"PostRebuildRatio":0}}}`, `Routes["claude"]: PostRebuildRatio`},
		{`{"Routes":{"claude":{"PostRebuildRatio":1}}}`, `Routes["claude"]: PostRebuildRatio`},
		{`{"Routes":{"claude":{"WindowThresholdTokens":9999}}}`, `Routes["claude"]: L2TargetTokens`},
		{`{"Routes":{"claude":{"Roles":{"lead":{"WindowThresholdTokens":9999}}}}}`, `Routes["claude"].Roles["lead"]: L2TargetTokens`},
		{`{"Routes":{"claude":{"Roles":{"lead":{"InputBudget":0}}}}}`, `Routes["claude"].Roles["lead"]: context: invalid_budget`},
	} {
		var cfg Config
		err := json.Unmarshal([]byte(test.raw), &cfg)
		if err == nil {
			_, err = cfg.Resolve()
		}
		if err == nil || !strings.Contains(err.Error(), test.field) {
			t.Errorf("%s accepted or returned the wrong error: %v", test.raw, err)
		}
	}
	cfg := Config{Routes: map[string]RouteConfig{"claude": {WindowThresholdTokens: new(int64(10_001)), L2TargetTokens: new(int64(5_000))}}}
	if _, err := cfg.Resolve(); err != nil {
		t.Fatal("L2 equal to the rounded target should be valid", err)
	}
}

func TestContextPolicyResolvesChildRoleOverrides(t *testing.T) {
	var config Config
	raw := []byte(`{"RecentEventCount":7,"Routes":{"opencode-go":{"PostRebuildRatio":0.6,"InputBudget":100000,"Roles":{
		"lead":{"WindowThresholdTokens":40000,"L2TargetTokens":2000,"RecentEventCount":0},
		"worker":{"ToolOutputExcerptChars":100}}}}}`)
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	lead := config.ForRole("opencode-go", "lead")
	if lead.WindowThresholdTokens != 40_000 || lead.PostRebuildRatio != 0.6 || lead.L2TargetTokens != 2000 ||
		lead.InputBudget != 100_000 || lead.RecentEventCount != 0 {
		t.Fatalf("lead policy = %+v", lead)
	}
	worker := config.ForRole("opencode-go", "worker")
	if worker.WindowThresholdTokens != 200_000 || worker.PostRebuildRatio != 0.6 || worker.L2TargetTokens != 5000 ||
		worker.RecentEventCount != 7 || worker.ToolOutputExcerptChars != 100 {
		t.Fatalf("worker policy = %+v", worker)
	}
	for _, route := range []string{"opencode-go", "claude"} {
		policy := config.ForProvider(route).windowPolicy()
		if config.ForRole(route, "collector").windowPolicy() != policy ||
			(route == "claude" && config.ForRole(route, "lead").windowPolicy() != policy) {
			t.Fatalf("role override leaked into %s", route)
		}
	}
	snapshot := config.PoliciesForRole("lead")
	if snapshot.Role != "lead" || snapshot.ForProvider("opencode-go") != lead.windowPolicy() {
		t.Fatalf("recorded policy = %+v", snapshot)
	}
}

func TestContextConfigFileKeepsLastValidPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	file, err := NewConfigFile(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"Runtime":{"irrelevant":"startup policy"},"Context":{"Routes":{"claude":{"WindowThresholdTokens":180000,"PostRebuildRatio":0.6}}}}`)
	before := file.Current().Policies().ForProvider("claude")
	if before.WindowThresholdTokens != 180_000 || before.PostRebuildRatio != 0.6 {
		t.Fatalf("valid change was not loaded: %+v", before)
	}
	for _, raw := range []string{`{"Context":`, `{"Context":{"Routes":{"claude":{"PostRebuildRatio":1}}}}`} {
		write(raw)
		if got := file.Current().Policies().ForProvider("claude"); got != before {
			t.Fatalf("invalid edit replaced the live policy: %+v", got)
		}
	}
	write(`{"Runtime":{"irrelevant":"startup policy"}}`)
	if got := file.Current().Policies().ForProvider("claude"); got.WindowThresholdTokens != 200_000 || got.PostRebuildRatio != 0.5 {
		t.Fatalf("removing the override did not restore defaults: %+v", got)
	}
}
