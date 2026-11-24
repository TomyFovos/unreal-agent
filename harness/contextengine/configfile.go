package contextengine

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

func (c *Config) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	_, err := c.DecodeJSON(dec)
	return err
}

// DecodeJSON accepts the retired options and reports whether WindowStrategy
// was present, so the launcher's initial load can log its deprecation.
func (c *Config) DecodeJSON(dec *jsontext.Decoder) (deprecated bool, err error) {
	type fields Config
	var decoded struct {
		fields
		WindowThresholdTokens        *int64
		PostRebuildRatio             *float64
		WindowStrategy               jsontext.Value
		ClaudeCompactThresholdTokens int64
	}
	if err := json.UnmarshalDecode(dec, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return false, err
	}
	*c = Config(decoded.fields)
	if w := decoded.WindowThresholdTokens; w != nil {
		if *w <= 0 {
			return false, fmt.Errorf("WindowThresholdTokens must be positive")
		}
		c.WindowThresholdTokens = *w
	}
	if ratio := decoded.PostRebuildRatio; ratio != nil {
		if !(0 < *ratio && *ratio < 1) {
			return false, fmt.Errorf("PostRebuildRatio must be between 0 and 1 (exclusive)")
		}
		c.PostRebuildRatio = *ratio
	}
	return len(decoded.WindowStrategy) != 0, nil
}

// ConfigFile reads only Context from the operator's runtime JSON. A process
// shares its last valid policy across its Sessions during edits.
type ConfigFile struct {
	path string

	mu        sync.Mutex
	raw       jsontext.Value
	current   Config
	lastError string
}

func NewConfigFile(path string, initial Config) (*ConfigFile, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	initial, err = initial.Resolve()
	if err != nil {
		return nil, err
	}
	return &ConfigFile{path: path, current: initial}, nil
}

func (f *ConfigFile) Path() string { return f.path }

// Current returns a resolved, read-only config. Missing or invalid files
// leave the last valid policy in effect.
func (f *ConfigFile) Current() Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reload(); err != nil {
		if message := err.Error(); message != f.lastError {
			slog.Warn("Context policy reload failed; keeping the current policy", "error", err)
			f.lastError = message
		}
	} else {
		f.lastError = ""
	}
	return f.current
}

func (f *ConfigFile) reload() error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	var document struct{ Context jsontext.Value }
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	raw := document.Context
	if len(raw) == 0 {
		raw = jsontext.Value("{}")
	}
	if bytes.Equal(raw, f.raw) {
		return nil
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	config, err = config.Resolve()
	if err != nil {
		return err
	}
	f.raw, f.current = raw, config
	return nil
}
