package modelcatalog

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const codexCatalogSource = "Codex CLI model/list (bundled or cached metadata; access unverified)"

// CodexCLI is a metadata-only reader for an explicitly opted-in external CLI
// source. It owns no authentication, threads, turns, tools or Session state.
// Native ExternalCodex keeps its existing cache-only discovery contract.
type CodexCLI struct {
	Home     string // same directory as the selected external auth.json
	Binary   string
	Getenv   func(string) string
	Validate func(context.Context) error // recheck the explicit OAuth proxy route
	// CachePath keeps the last model list across processes: the CLI can take
	// 10-30 seconds to start in a sandbox, and every child and Host would
	// otherwise wait for it again. Empty selects the user cache directory.
	CachePath string
	mu        sync.Mutex
	cached    Catalog
	expires   time.Time
}

func (c *CodexCLI) Catalog(ctx context.Context, refresh bool) Catalog {
	cache := ReadCodexCache(filepath.Join(c.Home, "models_cache.json"))
	if cache.Available {
		return cache
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := func(code string) Catalog {
		return Catalog{Source: codexCatalogSource, Problem: "catalog unavailable", ProblemCode: code}
	}
	if ctx.Err() != nil {
		return fail("codex_cli_canceled")
	}
	if c.Validate == nil || c.Validate(ctx) != nil {
		return fail("codex_oauth_source_invalid")
	}
	if !refresh && time.Now().Before(c.expires) {
		return c.cached.Clone()
	}
	saved, haveSaved := c.readSaved()
	if !refresh && haveSaved && time.Since(saved.FetchedAt) < 24*time.Hour {
		c.cached, c.expires = saved.Clone(), time.Now().Add(time.Minute)
		return saved
	}
	result := c.discover(ctx)
	if result.Available {
		c.save(result)
	} else if haveSaved {
		result = saved // a slow or failed start keeps the last good list
	}
	c.cached, c.expires = result.Clone(), time.Now().Add(time.Minute)
	return result
}

func (c *CodexCLI) cachePath() string {
	if c.CachePath != "" {
		return c.CachePath
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "unreal-agent", "codex-cli-catalog.json")
}

func (c *CodexCLI) readSaved() (Catalog, bool) {
	path := c.cachePath()
	if path == "" {
		return Catalog{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, false
	}
	var saved struct {
		Home    string
		Catalog Catalog
	}
	if json.Unmarshal(data, &saved) != nil || saved.Home != c.Home || !saved.Catalog.Available || len(saved.Catalog.Models) == 0 {
		return Catalog{}, false
	}
	return saved.Catalog, true
}

func (c *CodexCLI) save(catalog Catalog) {
	path := c.cachePath()
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	data, err := json.Marshal(struct {
		Home    string
		Catalog Catalog
	}{c.Home, catalog})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// Argv has no prompt, model override, provider override, credential value,
// thread/resume option, or user-configured command. CLI runtime DB/log writes
// are confined to the temporary metadata process directory.
func codexCatalogArguments(directory string) []string {
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []string{"app-server", "--listen", "stdio://",
		"-c", "analytics.enabled=false", "-c", "feedback.enabled=false",
		"-c", "features.hooks=false", "-c", "features.shell_tool=false", "-c", "features.apps=false", "-c", "features.remote_control=false",
		"-c", `history.persistence="none"`, "-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`,
		"-c", "sqlite_home=" + quote(directory), "-c", "log_dir=" + quote(directory)}
}

func (c *CodexCLI) discover(ctx context.Context) Catalog {
	fail := func(code string) Catalog {
		return Catalog{Source: codexCatalogSource, Problem: "catalog unavailable", ProblemCode: code}
	}
	if !filepath.IsAbs(c.Home) {
		return fail("codex_oauth_source_invalid")
	}
	name := c.Binary
	if name == "" {
		name = "codex"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fail("codex_cli_missing")
	}
	getenv := c.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// No API keys, tokens, inherited proxy headers, LD_PRELOAD, NODE_OPTIONS,
	// credentials of other providers, or full parent environment are forwarded.
	env := []string{"HOME=" + getenv("HOME"), "PATH=" + getenv("PATH"), "CODEX_HOME=" + c.Home, "NO_COLOR=1"}
	if getenv("SBX_CRED_OPENAI_MODE") == "oauth" {
		env = append(env, "SBX_CRED_OPENAI_MODE=oauth")
	}
	directory, err := os.MkdirTemp("", "unreal-codex-catalog-")
	if err != nil {
		return fail("codex_cli_start_failed")
	}
	defer os.RemoveAll(directory)
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(processCtx, path, codexCatalogArguments(directory)...)
	cmd.Dir, cmd.Env, cmd.Stderr = directory, env, io.Discard
	isolateCodexCatalog(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		return fail("codex_cli_start_failed")
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return fail("codex_cli_start_failed")
	}
	defer output.Close()
	if cmd.Start() != nil {
		// Cancellation can arrive during discovery setup, after Catalog's
		// initial context check but before exec starts the process.
		if ctx.Err() != nil {
			return fail("codex_cli_canceled")
		}
		return fail("codex_cli_start_failed")
	}
	result, err := readCodexCatalog(output, input)
	// Terminate even a CLI which holds stdin open or spawns a wrapper child.
	cancel()
	stopCodexCatalog(cmd)
	_ = cmd.Wait()
	if ctx.Err() != nil {
		return fail("codex_cli_canceled")
	}
	if err != nil {
		var failure *codexCatalogError
		if errors.As(err, &failure) {
			return fail(failure.code)
		}
		return fail("codex_cli_protocol_invalid")
	}
	return result
}

type codexCatalogError struct{ code string }

func (e *codexCatalogError) Error() string { return e.code }

func codexCatalogFailure(code string) error { return &codexCatalogError{code: code} }

// Only initialize, initialized and paginated model/list are written. Server
// requests and malformed/duplicate JSON return errors; notifications are ignored.
func readCodexCatalog(output io.Reader, input io.Writer) (Catalog, error) {
	scanner := bufio.NewReader(output)
	send := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = input.Write(append(data, '\n'))
		return err
	}
	request := func(id int, method string, params any) error {
		return send(map[string]any{"id": id, "method": method, "params": params})
	}
	receive := func(id int) (jsontext.Value, error) {
		for {
			data, err := scanner.ReadBytes('\n')
			if err != nil && len(data) == 0 {
				return nil, fmt.Errorf("read Codex catalog: %w", err)
			}
			var frame map[string]jsontext.Value
			if json.Unmarshal(data, &frame) != nil || frame == nil {
				return nil, codexCatalogFailure("codex_cli_protocol_invalid")
			}
			if method, ok := frame["method"]; ok {
				var name string
				if _, hasID := frame["id"]; hasID || json.Unmarshal(method, &name) != nil {
					return nil, codexCatalogFailure("codex_cli_protocol_invalid")
				}
				continue // opaque status metadata; never publish its params
			}
			var got int
			if json.Unmarshal(frame["id"], &got) != nil || got != id {
				return nil, codexCatalogFailure("codex_cli_protocol_invalid")
			}
			if _, ok := frame["error"]; ok {
				return nil, codexCatalogFailure("codex_cli_rejected")
			}
			value, ok := frame["result"]
			if !ok || string(value) == "null" {
				return nil, codexCatalogFailure("codex_cli_protocol_invalid")
			}
			return value, nil
		}
	}
	if err := request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "unreal_agent_catalog", "title": "Unreal Agent model catalog", "version": "1"}, "capabilities": map[string]any{"experimentalApi": false, "explicitGatewayOauth": true, "requestAttestation": false}}); err != nil {
		return Catalog{}, err
	}
	initialized, err := receive(1)
	if err != nil {
		return Catalog{}, err
	}
	var initObject map[string]jsontext.Value
	if json.Unmarshal(initialized, &initObject) != nil || initObject == nil {
		return Catalog{}, codexCatalogFailure("codex_cli_protocol_invalid")
	}
	if err := send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return Catalog{}, err
	}
	var rows []codexModelInfo
	var cursor *string
	seenCursor := map[string]bool{}
	for page := 0; ; page++ {
		if err := request(page+2, "model/list", map[string]any{"limit": 64, "includeHidden": false, "cursor": cursor}); err != nil {
			return Catalog{}, err
		}
		data, err := receive(page + 2)
		if err != nil {
			return Catalog{}, err
		}
		var result struct {
			Data       []codexModelInfo `json:"data"`
			NextCursor *string          `json:"nextCursor"`
		}
		if json.Unmarshal(data, &result) != nil || result.Data == nil {
			return Catalog{}, codexCatalogFailure("codex_cli_metadata_invalid")
		}
		rows = append(rows, result.Data...)
		if result.NextCursor == nil {
			return normalizeCodexModels(rows)
		}
		cursor = result.NextCursor
		if len(*cursor) == 0 || seenCursor[*cursor] {
			return Catalog{}, codexCatalogFailure("codex_cli_protocol_invalid")
		}
		seenCursor[*cursor] = true
	}
}

// Documented SDK status notifications can precede model/list. None is a
// command, approval or tool request; all params are discarded. In particular
// remoteControl status is not an authorization to connect or execute work.

type codexModelInfo struct {
	ID      string              `json:"id"`
	Model   string              `json:"model"`
	Name    string              `json:"displayName"`
	Hidden  *bool               `json:"hidden"`
	Default llm.ReasoningEffort `json:"defaultReasoningEffort"`
	Efforts []struct {
		Effort llm.ReasoningEffort `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
	IsDefault bool `json:"isDefault"`
}

func normalizeCodexModels(rows []codexModelInfo) (Catalog, error) {
	c := Catalog{Source: codexCatalogSource, FetchedAt: time.Now()}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].IsDefault && !rows[j].IsDefault })
	seen := map[string]bool{}
	for _, row := range rows {
		if !validID(row.Model) || strings.HasPrefix(row.Model, "-") || strings.ContainsAny(row.Name, "\x00\x1b\r\n") {
			continue
		}
		if row.Hidden != nil && *row.Hidden {
			continue
		}
		if seen[row.Model] {
			continue
		}
		seen[row.Model] = true
		m := Model{ID: row.Model, Name: row.Name, DefaultEffort: row.Default}
		if m.Name == "" {
			m.Name = m.ID
		}
		efforts := map[llm.ReasoningEffort]bool{}
		for _, option := range row.Efforts {
			if option.Effort == llm.ReasoningEffortUltra {
				continue
			}
			if !option.Effort.Valid() {
				m.UnsupportedEfforts = true
				continue
			}
			if !efforts[option.Effort] {
				m.Efforts = append(m.Efforts, option.Effort)
				efforts[option.Effort] = true
			}
		}
		if len(m.Efforts) == 0 {
			continue
		}
		m.DefaultEffort = m.RequestEffort(m.DefaultEffort)
		if !m.AllowsEffort(m.DefaultEffort) {
			m.DefaultEffort = m.Efforts[0]
		}
		c.Models = append(c.Models, m)
	}
	if len(c.Models) == 0 {
		return Catalog{}, codexCatalogFailure("codex_cli_metadata_invalid")
	}
	c.Available = true
	return c, nil
}
