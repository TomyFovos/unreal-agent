package main

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeConfigurationIgnoresRetiredWindowStrategy(t *testing.T) {
	for _, value := range []string{"", "\"summary\"", "\"three-layer\"", "\"unknown-old-value\"", "null"} {
		t.Run(value, func(t *testing.T) {
			raw := "{\"Context\":{\"InputBudget\":456789,\"ClaudeCompactThresholdTokens\":123456"
			if value != "" {
				raw += ",\"WindowStrategy\":" + value
			}
			raw += "},\"Claude\":{\"CompactThresholdTokens\":234567}}"
			path := filepath.Join(t.TempDir(), "runtime.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
			defer slog.SetDefault(logger)
			config, err := readServeConfiguration(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.Context.InputBudget != 456789 {
				t.Fatal("common context setting was lost")
			}
			encoded, err := json.Marshal(config)
			if err != nil || strings.Contains(string(encoded), "ThresholdTokens") || strings.Contains(string(encoded), "WindowStrategy") {
				t.Fatal("retired key reappeared in configuration", string(encoded), err)
			}
			want := 0
			if value != "" {
				want = 1
			}
			if strings.Count(log.String(), "\n") != want ||
				(want == 1 && !strings.Contains(log.String(), "Context.WindowStrategy is deprecated and ignored")) {
				t.Fatalf("unexpected deprecation log: %q", log.String())
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
