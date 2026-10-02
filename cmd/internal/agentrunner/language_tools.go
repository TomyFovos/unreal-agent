package agentrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

const languageServersEnvironment = "UNREAL_HARNESS_LSP_SERVERS"

// LanguageTools owns language servers for one Host and workspace. Session
// handlers borrow this owner. Close it after the Host has drained its sessions.
type LanguageTools struct {
	manager  *lsp.Manager
	enabled  bool
	identity string
}

func LanguageServerConfiguration(getenv func(string) string) ([]lsp.ServerConfig, error) {
	if getenv == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(getenv(languageServersEnvironment))
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 65536 {
		return nil, errors.New("language server configuration exceeds 64 KiB")
	}
	var servers []lsp.ServerConfig
	if err := json.Unmarshal([]byte(raw), &servers, json.RejectUnknownMembers(true)); err != nil {
		return nil, errors.New("invalid language server configuration")
	}
	return servers, nil
}

func NewLanguageTools(ctx context.Context, workspace string, servers []lsp.ServerConfig) (*LanguageTools, error) {
	files, err := mutation.New(mutation.Config{Root: workspace, Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		return nil, err
	}
	manager, err := lsp.New(ctx, lsp.Config{Mutation: files, Servers: servers})
	if err != nil {
		return nil, err
	}
	owner := &LanguageTools{manager: manager, enabled: len(servers) > 0}
	if owner.enabled {
		encoded, err := json.Marshal(servers)
		if err != nil {
			manager.Close()
			return nil, errors.New("invalid language server configuration")
		}
		sum := sha256.Sum256(encoded)
		owner.identity = hex.EncodeToString(sum[:])
	}
	return owner, nil
}
func (l *LanguageTools) Identity() string { return l.identity }
func (l *LanguageTools) Close() error     { return l.manager.Close() }

func (l *LanguageTools) Wrap(base ToolFactory, disallowed []string) ToolFactory {
	enabled := l.enabled && !slices.Contains(disallowed, lsp.ToolName)
	return func(ctx context.Context, config ToolConfig) (Tools, error) {
		configured, err := base(ctx, config)
		if err != nil {
			return Tools{}, err
		}
		cleanup := func() {
			if configured.Close != nil {
				_ = configured.Close()
			}
		}
		handler, err := lsp.NewHandler(ctx, l.manager, string(config.SessionID))
		if err != nil {
			cleanup()
			return Tools{}, err
		}
		registry, err := tool.WithExtensions(configured.Registry, []tool.Extension{{Definition: lsp.Definition(), Translator: lsp.Translator{}, Enabled: enabled}})
		if err != nil {
			handler.Close()
			cleanup()
			return Tools{}, err
		}
		closeBase := configured.Close
		configured.Registry = registry
		configured.RemoteJobs = append(configured.RemoteJobs, handler)
		configured.Close = func() error {
			err := handler.Close()
			if closeBase != nil {
				err = errors.Join(err, closeBase())
			}
			return err
		}
		return configured, nil
	}
}
