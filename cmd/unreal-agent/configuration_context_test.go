package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeConfigurationMovesLegacyClaudeThresholdToRoute(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want int64
	}{
		{"default", `{}`, 0},
		{"legacy", `{"Context":{"ClaudeCompactThresholdTokens":123456,"InputBudget":456789}}`, 123456},
		{"route", `{"Claude":{"CompactThresholdTokens":234567},"Context":{"InputBudget":456789}}`, 234567},
		{"route-wins", `{"Context":{"ClaudeCompactThresholdTokens":123456,"InputBudget":456789},"Claude":{"CompactThresholdTokens":234567}}`, 234567},
		{"legacy-default", `{"Context":{"ClaudeCompactThresholdTokens":-1,"InputBudget":456789}}`, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.json")
			if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := readServeConfiguration(path)
			if err != nil || config.Claude.CompactThresholdTokens != tc.want {
				t.Fatalf("route threshold = %d, want %d: %v", config.Claude.CompactThresholdTokens, tc.want, err)
			}
			if tc.name != "default" && config.Context.InputBudget != 456789 {
				t.Fatal("common context setting was lost")
			}
			encoded, err := json.Marshal(config)
			if err != nil || strings.Contains(string(encoded), "ClaudeCompactThresholdTokens") {
				t.Fatal("retired key was reintroduced into configuration", string(encoded), err)
			}
		})
	}
	for _, invalid := range []string{`{"Context":{"UnknownSetting":1}}`, `{"Context":{"ClaudeCompactThresholdTokens":"bad"}}`, `{"Claude":{"UnknownSetting":1}}`} {
		path := filepath.Join(t.TempDir(), "runtime.json")
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readServeConfiguration(path); err == nil {
			t.Errorf("accepted invalid configuration: %s", invalid)
		}
	}
}
