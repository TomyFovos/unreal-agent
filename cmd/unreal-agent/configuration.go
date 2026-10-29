package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
)

// Both entry points consume the same operator policy. The normal launcher may
// publish a conservative first-run configuration but never replaces a file.
func readServeConfiguration(path string) (serveConfiguration, error) {
	var config serveConfiguration
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("read runtime configuration: %w", err)
	}
	if err = json.Unmarshal(data, &config, json.RejectUnknownMembers(true)); err != nil {
		return config, fmt.Errorf("decode runtime configuration: %w", err)
	}
	if config.PermissionModes != nil {
		if err := config.PermissionModes.Validate(); err != nil {
			return config, fmt.Errorf("runtime permission modes: %w", err)
		}
	}
	return config, nil
}
