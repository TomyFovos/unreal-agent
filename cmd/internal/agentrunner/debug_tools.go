package agentrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	dapTool "github.com/unreallabsai/unreal-agent/harness/tool/dap"
)

const debugAdaptersEnvironment = "UNREAL_HARNESS_DAP_ADAPTERS"

func DebugAdapterConfiguration(getenv func(string) string) ([]dap.AdapterConfig, error) {
	if getenv == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(getenv(debugAdaptersEnvironment))
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 65536 {
		return nil, errors.New("debug adapter configuration exceeds 64 KiB")
	}
	var adapters []dap.AdapterConfig
	if json.Unmarshal([]byte(raw), &adapters, json.RejectUnknownMembers(true)) != nil {
		return nil, errors.New("invalid debug adapter configuration")
	}
	return adapters, nil
}

// DebugTools creates a fresh generation for each Host-owned Session runtime.
// Debugger handles never cross session ownership or survive runtime recreation.
func DebugTools(base ToolFactory, adapters []dap.AdapterConfig, disallowed []string) (ToolFactory, string, error) {
	if base == nil {
		return nil, "", errors.New("base tool factory is required")
	}
	encoded, err := json.Marshal(adapters)
	if err != nil {
		return nil, "", errors.New("invalid debug adapter configuration")
	}
	var configuredAdapters []dap.AdapterConfig
	if err = json.Unmarshal(encoded, &configuredAdapters); err != nil {
		return nil, "", err
	}
	// Configuration validation is pure; no adapter starts until an operation.
	probe, err := dap.NewManager(context.Background(), configuredAdapters...)
	if err != nil {
		return nil, "", err
	}
	probe.Close()
	enabled := len(configuredAdapters) > 0 && !slices.Contains(disallowed, dapTool.Name)
	identity := ""
	if len(configuredAdapters) > 0 {
		sum := sha256.Sum256(encoded)
		identity = hex.EncodeToString(sum[:])
	}
	factory := func(ctx context.Context, config ToolConfig) (Tools, error) {
		configured, err := base(ctx, config)
		if err != nil {
			return Tools{}, err
		}
		cleanup := func() {
			if configured.Close != nil {
				_ = configured.Close()
			}
		}
		manager, err := dap.NewManager(ctx, configuredAdapters...)
		if err != nil {
			cleanup()
			return Tools{}, err
		}
		registry, err := tool.WithExtensions(configured.Registry, []tool.Extension{{Definition: dapTool.Definition(), Translator: dapTool.New(manager.Generation()), Enabled: enabled}})
		if err != nil {
			manager.Close()
			cleanup()
			return Tools{}, err
		}
		closeBase := configured.Close
		configured.Registry = registry
		configured.RemoteJobs = append(configured.RemoteJobs, manager)
		configured.Close = func() error {
			err := manager.Close()
			if closeBase != nil {
				err = errors.Join(err, closeBase())
			}
			return err
		}
		return configured, nil
	}
	return factory, identity, nil
}
