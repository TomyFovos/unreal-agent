package openaicodex

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// DetectRoute is a read-only native extension to the existing source validator.
// A positive Docker result requires the complete source AND forward-proxy
// contract. Any Docker intent or ambiguous routing is fail-closed on mismatch;
// an available external credential never repairs it. A negative result still
// requires ExternalCodex to authenticate independently. No token is returned.
func (source DockerSandboxSource) DetectRoute() (bool, error) {
	getenv := source.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if source.AuthFile == "" {
		return false, &SandboxProxyError{}
	}
	docker := getenv("SBX_CRED_OPENAI_MODE") != ""
	configPath := filepath.Join(filepath.Dir(source.AuthFile), "config.toml")
	// Absence is checked before opening. A present source that disappears or
	// becomes unsafe during capture must not be reclassified as native routing.
	_, statErr := os.Lstat(configPath)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, &SandboxProxyError{}
	}
	if statErr == nil {
		data, err := sandboxFile(configPath, 256<<10)
		if err != nil {
			return false, &SandboxProxyError{}
		}
		var config map[string]any
		if toml.Unmarshal(data, &config) != nil || config == nil {
			return false, &SandboxProxyError{}
		}
		if name, exists := config["model_provider"]; exists {
			switch name {
			case "sandboxd":
				docker = true
			case "openai":
			default:
				return false, &SandboxProxyError{}
			}
		}
		if mode, exists := config["forced_login_method"]; exists {
			switch mode {
			case "api":
				docker = true // requires complete OAuth kit validation below
			case "chatgpt":
			default:
				return false, &SandboxProxyError{}
			}
		}
	}
	// A proxy marker without consistent kit routing cannot be used as a token.
	// Contents are transient and remain inside the native credential owner.
	if data, err := sandboxFile(source.AuthFile, 1<<20); err == nil {
		var auth struct {
			Marker string `json:"OPENAI_API_KEY"`
		}
		if json.Unmarshal(data, &auth) != nil {
			return false, &SandboxProxyError{}
		}
		if auth.Marker != "" {
			docker = true
		}
	}
	if !docker {
		return false, nil
	}
	if source.Validate() != nil {
		return false, &SandboxProxyError{}
	}
	if _, err := DockerSandboxProxyURL(getenv); err != nil {
		return false, &SandboxProxyError{}
	}
	return true, nil
}
