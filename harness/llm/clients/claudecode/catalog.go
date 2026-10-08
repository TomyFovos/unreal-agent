package claudecode

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

const catalogTTL = time.Minute
const discoveredCatalogSource = "Claude Code control catalog"

// Catalog is a read-only, process-lifetime cache. Neither account metadata nor
// CLI credentials are decoded. Only initialize/list_models are sent, never a
// user message, prompt or tool command. Upgrades invalidate the executable key;
// login/policy changes are refreshed within the short TTL or by InvalidateCatalog.
func (c *Client) Catalog(ctx context.Context) (modelcatalog.Catalog, error) {
	if c.config.CatalogSource != nil {
		return c.config.CatalogSource(ctx)
	}
	if err := ctx.Err(); err != nil {
		return modelcatalog.Catalog{}, err
	}
	env, err := c.environment()
	if err != nil {
		return modelcatalog.Catalog{}, err
	}
	name := c.config.Binary
	if name == "" {
		name = "claude"
	}
	path, _ := exec.LookPath(name)
	key := path + "\n" + strings.Join(env, "\n")
	if info, e := os.Stat(path); e == nil {
		key += "\n" + info.ModTime().String() + "\n" + strconv.FormatInt(info.Size(), 10)
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if err := ctx.Err(); err != nil {
		return modelcatalog.Catalog{}, err
	}
	if key == c.cacheKey && time.Now().Before(c.expires) {
		return c.cached.Clone(), nil
	}
	result, err := c.discoverCatalog(ctx)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) || e.Code != "catalog_unavailable" && e.Code != "binary_not_found" && e.Code != "unsupported_version" && e.Code != "subprocess_failure" {
			return modelcatalog.Catalog{}, err
		}
		result = c.catalog.Clone()
		result.Problem = "catalog discovery unavailable"
		if result.Available {
			result.Problem += "; using explicit configured catalog"
		}
	} else {
		// Operator display/default/context metadata may override an existing
		// returned row, but cannot add a model or expand its supported efforts.
		for i := range result.Models {
			m := &result.Models[i]
			for _, configured := range c.catalog.Models {
				if !m.Matches(configured.ID) {
					continue
				}
				if configured.Name != configured.ID {
					m.Name = configured.Name
				}
				if m.AllowsEffort(configured.DefaultEffort) {
					m.DefaultEffort = configured.DefaultEffort
				}
				if configured.ContextWindow > 0 {
					m.ContextWindow = configured.ContextWindow
				}
				break
			}
		}
	}
	c.cached, c.cacheKey, c.expires = result.Clone(), key, time.Now().Add(catalogTTL)
	return result.Clone(), nil
}

func (c *Client) InvalidateCatalog() {
	c.cacheMu.Lock()
	c.expires = time.Time{}
	c.cacheMu.Unlock()
}

func (c *Client) discoverCatalog(ctx context.Context) (modelcatalog.Catalog, error) {
	path, env, err := c.prepare(ctx)
	if err != nil {
		return modelcatalog.Catalog{}, err
	}
	dir, err := os.MkdirTemp("", "unreal-claude-catalog-")
	if err != nil {
		return modelcatalog.Catalog{}, &Error{Code: "subprocess_failure"}
	}
	defer os.RemoveAll(dir)
	// This is the official SDK stdio transport, with no -p/--print or prompt.
	args := append(isolationArgs(), "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence", "--permission-prompts", "none", "--max-turns", "1", "--system-prompt-file", os.DevNull)
	if err = validateLaunchContract(args); err != nil {
		return modelcatalog.Catalog{}, err
	}
	processCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(processCtx, path, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, env, io.Discard
	if err = isolateProcess(cmd); err != nil {
		return modelcatalog.Catalog{}, err
	}
	reader, output, err := os.Pipe()
	if err != nil {
		return modelcatalog.Catalog{}, &Error{Code: "subprocess_failure"}
	}
	defer reader.Close()
	defer output.Close()
	input, writer, err := os.Pipe()
	if err != nil {
		return modelcatalog.Catalog{}, &Error{Code: "subprocess_failure"}
	}
	defer input.Close()
	defer writer.Close()
	cmd.Stdout, cmd.Stdin = output, input
	if err = cmd.Start(); err != nil {
		return modelcatalog.Catalog{}, &Error{Code: "subprocess_failure"}
	}
	output.Close()
	input.Close()
	done, parsed := make(chan struct{}), make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = killProcessGroup(cmd)
		select {
		case <-parsed:
		case <-processCtx.Done():
			reader.Close()
		case <-time.After(2 * time.Second):
			reader.Close()
		}
		close(done)
	}()
	result, err := readCatalogControls(reader, writer)
	close(parsed)
	cancel()
	writer.Close()
	<-done
	if ctx.Err() != nil {
		return modelcatalog.Catalog{}, ctx.Err()
	}
	return result, err
}

type modelInfo struct {
	Value            string                `json:"value"`
	ResolvedModel    string                `json:"resolvedModel"`
	Name             string                `json:"displayName"`
	Description      string                `json:"description"`
	SupportsEffort   *bool                 `json:"supportsEffort"`
	Efforts          []llm.ReasoningEffort `json:"supportedEffortLevels"`
	AdaptiveThinking *bool                 `json:"supportsAdaptiveThinking"`
	FastMode         *bool                 `json:"supportsFastMode"`
}

func normalizeModels(raw []modelInfo) (modelcatalog.Catalog, error) {
	c := modelcatalog.Catalog{Source: discoveredCatalogSource, Available: true, Authoritative: true, FetchedAt: time.Now()}
	if raw == nil || len(raw) > 128 {
		return modelcatalog.Catalog{}, &Error{Code: "catalog_unavailable"}
	}
	seen := map[string]bool{}
	for _, info := range raw {
		if !publicID(info.Value) || strings.HasPrefix(info.Value, "-") || seen[info.Value] || info.ResolvedModel != "" && (!publicID(info.ResolvedModel) || strings.HasPrefix(info.ResolvedModel, "-")) || len(info.Name) > 256 || len(info.Description) > 4096 {
			return modelcatalog.Catalog{}, &Error{Code: "catalog_unavailable"}
		}
		seen[info.Value] = true
		m := modelcatalog.Model{ID: info.Value, ResolvedModel: info.ResolvedModel, Name: info.Name, Description: info.Description, SupportsEffort: info.SupportsEffort, AdaptiveThinking: info.AdaptiveThinking, FastMode: info.FastMode}
		if m.Name == "" {
			m.Name = m.ID
		}
		if info.SupportsEffort == nil || *info.SupportsEffort {
			levels := map[llm.ReasoningEffort]bool{}
			for _, level := range info.Efforts {
				if !level.Valid() {
					m.UnsupportedEfforts = true
					continue
				}
				if !levels[level] {
					m.Efforts = append(m.Efforts, level)
					levels[level] = true
				}
			}
			if len(m.Efforts) > 0 {
				m.DefaultEffort = m.Efforts[0]
			}
		}
		c.Models = append(c.Models, m)
	}
	return c.Clone(), nil
}

func writeCatalogControl(w io.Writer, id, subtype string) error {
	if subtype != "initialize" && subtype != "list_models" {
		return &Error{Code: "catalog_unavailable"}
	}
	request := map[string]any{"subtype": subtype}
	if subtype == "initialize" {
		request["hooks"] = map[string]any{}
		request["sdkMcpServers"] = []string{}
	}
	if json.MarshalWrite(w, map[string]any{"type": "control_request", "request_id": id, "request": request}) != nil {
		return &Error{Code: "catalog_unavailable"}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return &Error{Code: "catalog_unavailable"}
	}
	return nil
}

func readCatalogControls(r io.Reader, w io.Writer) (modelcatalog.Catalog, error) {
	if err := writeCatalogControl(w, "unreal_catalog_init", "initialize"); err != nil {
		return modelcatalog.Catalog{}, err
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 8192), 1<<20)
	var initial []modelInfo
	initialized, streamInitialized, total, count := false, false, 0, 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line)
		count++
		if total > 4<<20 || count > 256 {
			break
		}
		var v struct {
			Type     string `json:"type"`
			Response struct {
				Subtype string `json:"subtype"`
				ID      string `json:"request_id"`
				Payload struct {
					Models []modelInfo `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &v) != nil {
			break
		}
		if v.Type == "keep_alive" {
			continue
		}
		if v.Type == "system" {
			var record streamRecord
			if err := json.Unmarshal(line, &record); err != nil {
				return modelcatalog.Catalog{}, streamDecodeFailure(line, err, streamInitialized)
			}
			if record.ParentToolUseID != "" || record.SubagentType != "" || len(record.ToolUseResult) > 0 && string(record.ToolUseResult) != "null" {
				return modelcatalog.Catalog{}, toolStreamFailure()
			}
			if record.Subtype == "init" {
				if len(record.Tools) != 0 {
					return modelcatalog.Catalog{}, initializationFailure(streamInitToolsNonempty, len(record.Tools))
				}
				if len(record.MCP) != 0 {
					return modelcatalog.Catalog{}, initializationFailure(streamInitMCPNonempty, len(record.MCP))
				}
				if streamInitialized {
					return modelcatalog.Catalog{}, initializationFailure(streamInitDuplicate, 0)
				}
				if record.Tools == nil || record.MCP == nil || !publicID(record.Model) {
					return modelcatalog.Catalog{}, initRequiredFieldFailure(line, record)
				}
				if err := streamPermissionMode(record.PermissionMode, true); err != nil {
					return modelcatalog.Catalog{}, err
				}
				streamInitialized = true
				continue
			}
			return modelcatalog.Catalog{}, toolStreamFailure()
		}
		if v.Type != "control_response" {
			return modelcatalog.Catalog{}, toolStreamFailure()
		}
		if !initialized && v.Response.ID == "unreal_catalog_init" && v.Response.Subtype == "success" {
			initial, initialized = v.Response.Payload.Models, true
			if err := writeCatalogControl(w, "unreal_catalog_models", "list_models"); err != nil {
				break
			}
			continue
		}
		if initialized && v.Response.ID == "unreal_catalog_models" {
			if v.Response.Subtype == "success" {
				return normalizeModels(v.Response.Payload.Models)
			}
			if v.Response.Subtype == "error" {
				return normalizeModels(initial)
			}
		}
		break
	}
	return modelcatalog.Catalog{}, &Error{Code: "catalog_unavailable"}
}
