package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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
	// Decode the retired key only at the operator configuration boundary; it
	// must not remain a provider-specific setting in the shared context engine.
	var legacyThreshold int64
	type commonContext contextengine.Config
	legacyContext := json.UnmarshalFromFunc(func(dec *jsontext.Decoder, c *contextengine.Config) error {
		var legacy struct {
			commonContext
			ClaudeCompactThresholdTokens int64
		}
		if err := json.UnmarshalDecode(dec, &legacy); err != nil {
			return err
		}
		*c = contextengine.Config(legacy.commonContext)
		legacyThreshold = legacy.ClaudeCompactThresholdTokens
		return nil
	})
	if err = json.Unmarshal(data, &config, json.RejectUnknownMembers(true), json.WithUnmarshalers(legacyContext)); err != nil {
		return config, fmt.Errorf("decode runtime configuration: %w", err)
	}
	if config.Claude.CompactThresholdTokens == 0 {
		config.Claude.CompactThresholdTokens = legacyThreshold
	}
	if config.PermissionModes != nil {
		if err := config.PermissionModes.Validate(); err != nil {
			return config, fmt.Errorf("runtime permission modes: %w", err)
		}
	}
	return config, nil
}
