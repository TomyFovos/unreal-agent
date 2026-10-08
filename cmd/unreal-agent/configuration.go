package main

import (
	"encoding/json/v2"
	"io"
	"os"
)

// Both entry points consume the same operator policy. The normal launcher may
// publish a conservative first-run configuration but never replaces a file.
func readServeConfiguration(path string) (serveConfiguration, error) {
	var config serveConfiguration
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return config, err
	}
	if len(data) > 1<<20 {
		return config, &configurationError{Code: "configuration_too_large"}
	}
	if err = json.Unmarshal(data, &config, json.RejectUnknownMembers(true)); err != nil {
		return config, &configurationError{Code: "invalid_runtime_configuration"}
	}
	return config, nil
}

type configurationError struct{ Code string }

func (e *configurationError) Error() string {
	if e.Code == "configuration_too_large" {
		return "configuration exceeds 1 MiB"
	}
	return "invalid runtime configuration JSON"
}
