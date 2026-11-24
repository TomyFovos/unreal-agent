package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
)

// Both entry points consume the same operator policy. The normal launcher may
// publish a conservative first-run configuration but never replaces a file.
func readServeConfiguration(path string) (serveConfiguration, error) {
	var config serveConfiguration
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("read runtime configuration: %w", err)
	}
	// Newly written configurations contain only the current policy.
	var windowStrategy bool
	legacyContext := json.UnmarshalFromFunc(func(dec *jsontext.Decoder, c *contextengine.Config) error {
		var err error
		windowStrategy, err = c.DecodeJSON(dec)
		return err
	})
	var compatible struct {
		serveConfiguration
		Claude struct{ CompactThresholdTokens int64 }
	}
	if err = json.Unmarshal(data, &compatible, json.RejectUnknownMembers(true), json.WithUnmarshalers(legacyContext)); err != nil {
		return config, fmt.Errorf("decode runtime configuration: %w", err)
	}
	config = compatible.serveConfiguration
	if windowStrategy {
		slog.Warn("Context.WindowStrategy is deprecated and ignored. Unreal uses three-layer context.")
	}
	if _, err := config.Context.Resolve(); err != nil {
		return config, fmt.Errorf("runtime context: %w", err)
	}
	if config.PermissionModes != nil {
		if err := config.PermissionModes.Validate(); err != nil {
			return config, fmt.Errorf("runtime permission modes: %w", err)
		}
	}
	return config, nil
}
