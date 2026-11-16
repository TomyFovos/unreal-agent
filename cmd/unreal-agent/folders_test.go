//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// foldersFixture is one folders command over a private HOME. Every launcher
// seam is recorded and the folder's Claude runtime is faked, so nothing here
// starts a Host, binds a socket, runs discovery or reads the real HOME.
type foldersFixture struct {
	t        *testing.T
	command  foldersCommand
	env      map[string]string
	home     string
	output   bytes.Buffer
	launches [][]string
	chdirs   []string
	runtimes []launchOptions
	identity agentrunner.RuntimeIdentity
}

func newFoldersFixture(t *testing.T) *foldersFixture {
	t.Helper()
	// /tmp keeps the macOS socket path budget, and the resolved path keeps the
	// ~ substitution comparable with folderFor's own resolution.
	f := &foldersFixture{t: t, home: foldersReal(t, privateCLIDirectory(t)), env: map[string]string{}}
	f.env["HOME"] = f.home
	f.identity = foldersClaudeIdentity()
	f.command = foldersCommand{
		input:  strings.NewReader(""),
		output: &f.output,
		getenv: func(key string) string { return f.env[key] },
		launch: func(_ context.Context, args []string, _ io.Writer) error {
			f.launches = append(f.launches, slices.Clone(args))
			return nil
		},
		chdir: func(path string) error {
			f.chdirs = append(f.chdirs, path)
			return nil
		},
		claudeRuntime: func(_ context.Context, options launchOptions) (agentrunner.RuntimeIdentity, error) {
			f.runtimes = append(f.runtimes, options)
			return f.identity, nil
		},
	}
	return f
}

func (f *foldersFixture) run(args ...string) error {
	f.t.Helper()
	return f.command.run(f.t.Context(), args)
}

func (f *foldersFixture) stdin(text string) { f.command.input = strings.NewReader(text) }

func (f *foldersFixture) mkdir(name string) string {
	f.t.Helper()
	path := filepath.Join(f.home, name)
	if err := os.Mkdir(path, 0700); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *foldersFixture) defaultConfigPath() string {
	return filepath.Join(f.home, ".config", "unreal-agent", folderConfigName)
}

func (f *foldersFixture) defaultState() string {
	return filepath.Join(f.home, ".local", "state", "unreal-agent")
}

// writeDefaultConfig publishes HOME/.config/unreal-agent/runtime.json.
func (f *foldersFixture) writeDefaultConfig(config serveConfiguration) []byte {
	f.t.Helper()
	data, err := json.Marshal(config, jsontext.WithIndent("  "))
	if err != nil {
		f.t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(f.defaultConfigPath()), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.defaultConfigPath(), data, 0600); err != nil {
		f.t.Fatal(err)
	}
	return data
}

// writeSessions puts one Session file per mtime into a state directory.
func (f *foldersFixture) writeSessions(state string, sessions map[string]time.Time) {
	f.t.Helper()
	for id, updated := range sessions {
		directory := filepath.Join(state, "sessions")
		if err := os.MkdirAll(directory, 0700); err != nil {
			f.t.Fatal(err)
		}
		path := filepath.Join(directory, string(session.ID(id))+".session.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Chtimes(path, updated, updated); err != nil {
			f.t.Fatal(err)
		}
	}
}

// addFolder is another known folder: its own config/state roots, its
// folder.json and its Sessions. The per-folder configuration directory is what
// makes it known; the configuration file itself need not exist yet.
func (f *foldersFixture) addFolder(name string, sessions map[string]time.Time) folderEntry {
	f.t.Helper()
	folder := foldersReal(f.t, f.mkdir(name))
	entry := folderEntry{
		Folder: folder,
		Config: filepath.Join(f.home, ".config", "unreal-agent", foldersDirectoryName, folderID(folder), folderConfigName),
		State:  filepath.Join(f.home, ".local", "state", "unreal-agent", foldersDirectoryName, folderID(folder)),
	}
	if err := os.MkdirAll(filepath.Dir(entry.Config), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(entry.State, 0700); err != nil {
		f.t.Fatal(err)
	}
	record, err := json.Marshal(folderRecord{Version: 1, Workspace: folder})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry.State, folderRecordName), record, 0600); err != nil {
		f.t.Fatal(err)
	}
	f.writeSessions(entry.State, sessions)
	return entry
}

// listedLines is the numbered part of the output, in order.
func (f *foldersFixture) listedLines() []string {
	var lines []string
	for _, line := range strings.Split(f.output.String(), "\n") {
		if len(line) > 2 && line[0] >= '0' && line[0] <= '9' && strings.Contains(line, ") ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// foldersReal is the path folderFor reports for a directory: symlinks resolved.
func foldersReal(t *testing.T, directory string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func foldersClaudeIdentity() agentrunner.RuntimeIdentity {
	return agentrunner.RuntimeIdentity{
		Version: 1,
		Provider: provider.Selection{
			Version:     1,
			Provider:    claudeagent.ProviderID,
			Model:       provider.Model{ID: "claude-opus-4-6", Family: "claude"},
			Endpoint:    "https://api.anthropic.com",
			Auth:        credential.Reference{Provider: claudeagent.ProviderID, Method: credential.OAuth, ID: "claude-cli"},
			MaxAttempts: 1,
			Source:      "first-run discovery",
		},
		Profile:         profile.Default(),
		Workspace:       "/discovered/current",
		SystemPrompt:    "built claude prompt",
		ReasoningEffort: llm.ReasoningEffortHigh,
		Claude:          &claudeagent.Config{Binary: "/opt/identified-claude"},
	}
}

// foldersConfiguration is a default runtime.json with the workspace wired into
// every field that can hold it, and other values that must stay unchanged.
func foldersConfiguration(workspace string) serveConfiguration {
	maximum := permission.Config{Tools: []string{"read", "grep"}, FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted}
	worker := childTemplate{
		Runtime:     agentrunner.RuntimeIdentity{Version: 1, Workspace: workspace, SystemPrompt: "worker prompt"},
		Permissions: permission.Config{Tools: []string{"read"}, ReadRoots: []string{workspace}},
		Baseline:    &permission.Config{Tools: []string{"read"}, ReadRoots: []string{workspace}},
	}
	lead := childTemplate{
		Runtime:     agentrunner.RuntimeIdentity{Version: 1, Workspace: workspace, SystemPrompt: "lead prompt"},
		Permissions: permission.Config{Tools: []string{"read"}, ReadRoots: []string{workspace}, WriteRoots: []string{workspace}},
		Workers:     map[string]childTemplate{"worker": worker},
	}
	return serveConfiguration{
		Runtime: agentrunner.RuntimeIdentity{
			Version:         1,
			Provider:        provider.Selection{Version: 1, Provider: "openai-codex", Model: provider.Model{ID: "gpt-6.1-sol"}, Endpoint: "https://codex.invalid", Auth: credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}, MaxAttempts: 1, Source: "operator configuration"},
			Profile:         profile.Default(),
			Workspace:       workspace,
			SystemPrompt:    "Help with this workspace.",
			ReasoningEffort: llm.ReasoningEffortLow,
			DisallowedTools: []string{"SubagentStart"},
			Claude:          &claudeagent.Config{Binary: "/opt/claude"},
		},
		Permissions: permission.Config{
			Tools:          []string{"read", "grep", "glob"},
			ReadRoots:      []string{workspace, "/shared/data", workspace},
			WriteRoots:     []string{workspace, "/shared/out"},
			NetworkOrigins: []string{"https://codex.invalid"},
			ProcessMode:    permission.ProcessDenied,
		},
		PermissionModes: &permission.ModesConfig{Enabled: true, Maximum: &maximum},
		LanguageServers: []lsp.ServerConfig{
			{Language: "go", Path: workspace, Arguments: []string{workspace, "serve"}, Environment: []string{"GOWORK=" + workspace}},
			{Language: "rust", Path: workspace + "/bin/rust-analyzer", Arguments: []string{"--root", workspace + "/crate"}, Environment: []string{"CARGO_HOME=" + workspace + "/.cargo"}},
		},
		DebugAdapters: []dap.AdapterConfig{
			{ID: "dlv", Path: workspace, Directory: workspace, Arguments: []string{workspace}, Environment: []string{"WS=" + workspace}},
			{ID: "lldb", Path: workspace + "/bin/lldb", Directory: workspace + "/out", Arguments: []string{"--dir", workspace + "/out"}},
		},
		Subagents: map[string]childTemplate{"lead": lead},
	}
}

func TestFoldersFolderDetection(t *testing.T) {
	root := foldersReal(t, privateCLIDirectory(t))
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := folderFor(plain)
	if err != nil || got != plain {
		t.Fatalf("plain folder = %q, %v; want %q", got, err, plain)
	}

	// A directory inside a git repository belongs to its top level.
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repository, "init", "-q").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	nested := filepath.Join(repository, "internal", "package")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if got, err = folderFor(nested); err != nil || got != foldersReal(t, repository) {
		t.Fatalf("git folder = %q, %v; want %q", got, err, repository)
	}

	// A symlinked path resolves to the real directory.
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, err = folderFor(link); err != nil || got != foldersReal(t, target) {
		t.Fatalf("symlinked folder = %q, %v; want %q", got, err, target)
	}
}

func TestFoldersFolderID(t *testing.T) {
	// The same key scripts/common_development.py computes.
	for folder, want := range map[string]string{
		"/Users/example/project": "r-cdf51c85823de9ce",
		"/tmp/folder":            "r-04bd648861b09d50",
	} {
		if got := folderID(folder); got != want {
			t.Fatalf("folderID(%q) = %q, want %q", folder, got, want)
		}
	}
}

func TestFoldersConfigurationRelocation(t *testing.T) {
	root := foldersReal(t, privateCLIDirectory(t))
	workspace := filepath.Join(root, "workspace")
	folder := filepath.Join(root, "next")
	config := foldersConfiguration(workspace)
	relocated := relocateConfiguration(config, workspace, folder)

	if relocated.Runtime.Workspace != folder {
		t.Fatalf("workspace = %q, want %q", relocated.Runtime.Workspace, folder)
	}
	if want := []string{folder, "/shared/data", folder}; !slices.Equal(relocated.Permissions.ReadRoots, want) {
		t.Fatalf("read roots = %q, want %q", relocated.Permissions.ReadRoots, want)
	}
	if want := []string{folder, "/shared/out"}; !slices.Equal(relocated.Permissions.WriteRoots, want) {
		t.Fatalf("write roots = %q, want %q", relocated.Permissions.WriteRoots, want)
	}
	// Every other permission key and every mode stays exactly as configured.
	if !slices.Equal(relocated.Permissions.Tools, config.Permissions.Tools) ||
		!slices.Equal(relocated.Permissions.NetworkOrigins, config.Permissions.NetworkOrigins) ||
		relocated.Permissions.FilesystemUnrestricted != config.Permissions.FilesystemUnrestricted ||
		relocated.Permissions.NetworkUnrestricted != config.Permissions.NetworkUnrestricted ||
		relocated.Permissions.ProcessMode != config.Permissions.ProcessMode {
		t.Fatalf("permissions changed: %+v vs %+v", relocated.Permissions, config.Permissions)
	}
	if !reflect.DeepEqual(relocated.PermissionModes, config.PermissionModes) {
		t.Fatalf("permission modes changed: %+v", relocated.PermissionModes)
	}
	if !reflect.DeepEqual(relocated.Launcher, config.Launcher) || !reflect.DeepEqual(relocated.Context, config.Context) ||
		!reflect.DeepEqual(relocated.Providers, config.Providers) {
		t.Fatal("configuration without a workspace path changed")
	}
	// Language servers and debug adapters name the folder only where the whole
	// value is the old workspace; a prefix is not a match.
	servers := relocated.LanguageServers
	if servers[0].Path != folder || servers[0].Arguments[0] != folder || servers[0].Environment[0] != "GOWORK="+workspace {
		t.Fatalf("language server = %+v", servers[0])
	}
	if servers[1].Path != workspace+"/bin/rust-analyzer" || !slices.Equal(servers[1].Arguments, []string{"--root", workspace + "/crate"}) {
		t.Fatalf("sibling language server rewritten: %+v", servers[1])
	}
	adapters := relocated.DebugAdapters
	if adapters[0].Path != folder || adapters[0].Directory != folder || adapters[0].Arguments[0] != folder || adapters[0].Environment[0] != "WS="+workspace {
		t.Fatalf("debug adapter = %+v", adapters[0])
	}
	if adapters[1].Path != workspace+"/bin/lldb" || adapters[1].Directory != workspace+"/out" {
		t.Fatalf("sibling debug adapter rewritten: %+v", adapters[1])
	}
	// Child templates, their permissions, baselines and workers follow.
	lead := relocated.Subagents["lead"]
	worker := lead.Workers["worker"]
	if lead.Runtime.Workspace != folder || !slices.Equal(lead.Permissions.ReadRoots, []string{folder}) || !slices.Equal(lead.Permissions.WriteRoots, []string{folder}) {
		t.Fatalf("lead template = %+v", lead)
	}
	if worker.Runtime.Workspace != folder || worker.Baseline == nil || !slices.Equal(worker.Baseline.ReadRoots, []string{folder}) {
		t.Fatalf("worker template = %+v", worker)
	}
	// The original configuration is unchanged: it is copied, not edited.
	if config.Runtime.Workspace != workspace || config.Subagents["lead"].Runtime.Workspace != workspace {
		t.Fatal("the default configuration was edited")
	}
	if relocatePath(workspace+"/sub", workspace, folder) != workspace+"/sub" || relocatePath("", workspace, folder) != "" {
		t.Fatal("relocatePath replaced something other than the exact workspace")
	}
	if relocatePaths(nil, workspace, folder) != nil {
		t.Fatal("relocatePaths invented an empty slice")
	}
}

func TestFoldersConfigurationRelocationPermissionModes(t *testing.T) {
	root := foldersReal(t, privateCLIDirectory(t))
	workspace := filepath.Join(root, "workspace")
	folder := filepath.Join(root, "next")
	config := foldersConfiguration(workspace)
	maximum := permission.Config{
		Tools:                  []string{"read", "grep", "Write"},
		ReadRoots:              []string{workspace, "/shared/data"},
		WriteRoots:             []string{workspace, "/shared/out"},
		NetworkOrigins:         []string{"https://codex.invalid"},
		FilesystemUnrestricted: true,
		NetworkUnrestricted:    true,
		ProcessMode:            permission.ProcessDenied,
	}
	modes := permission.ModesConfig{ApprovalTimeoutMillis: 1500, Enabled: true, Maximum: &maximum}
	config.PermissionModes = &modes
	relocated := relocateConfiguration(config, workspace, folder)

	relocatedModes := relocated.PermissionModes
	if relocatedModes == nil || relocatedModes.Maximum == nil {
		t.Fatalf("permission modes = %+v", relocatedModes)
	}
	// The maximum follows the folder, and only the exact workspace root moves.
	if want := []string{folder, "/shared/data"}; !slices.Equal(relocatedModes.Maximum.ReadRoots, want) {
		t.Fatalf("maximum read roots = %q, want %q", relocatedModes.Maximum.ReadRoots, want)
	}
	if want := []string{folder, "/shared/out"}; !slices.Equal(relocatedModes.Maximum.WriteRoots, want) {
		t.Fatalf("maximum write roots = %q, want %q", relocatedModes.Maximum.WriteRoots, want)
	}
	if relocatedModes.ApprovalTimeoutMillis != modes.ApprovalTimeoutMillis || relocatedModes.Enabled != modes.Enabled ||
		!slices.Equal(relocatedModes.Maximum.Tools, maximum.Tools) ||
		!slices.Equal(relocatedModes.Maximum.NetworkOrigins, maximum.NetworkOrigins) ||
		relocatedModes.Maximum.FilesystemUnrestricted != maximum.FilesystemUnrestricted ||
		relocatedModes.Maximum.NetworkUnrestricted != maximum.NetworkUnrestricted ||
		relocatedModes.Maximum.ProcessMode != maximum.ProcessMode {
		t.Fatalf("permission modes changed: %+v", relocatedModes)
	}
	// A copy, never an alias of the default configuration's modes or maximum.
	if relocatedModes == config.PermissionModes || relocatedModes.Maximum == config.PermissionModes.Maximum {
		t.Fatal("relocated permission modes alias the default configuration")
	}
	// The source maximum is unchanged: it is copied, not edited.
	if modes.Maximum != &maximum || !slices.Equal(maximum.ReadRoots, []string{workspace, "/shared/data"}) ||
		!slices.Equal(maximum.WriteRoots, []string{workspace, "/shared/out"}) {
		t.Fatalf("the default permission modes were edited: %+v", modes)
	}
	// Nil PermissionModes and a nil Maximum stay nil.
	if relocated := relocateConfiguration(serveConfiguration{}, workspace, folder); relocated.PermissionModes != nil {
		t.Fatalf("permission modes appeared: %+v", relocated.PermissionModes)
	}
	bare := foldersConfiguration(workspace)
	bare.PermissionModes = &permission.ModesConfig{Enabled: true}
	if relocated := relocateConfiguration(bare, workspace, folder); relocated.PermissionModes == nil || relocated.PermissionModes.Maximum != nil {
		t.Fatalf("a maximum appeared: %+v", relocated.PermissionModes)
	}
}

func TestFoldersCreateConfiguration(t *testing.T) {
	f := newFoldersFixture(t)
	workspace := f.mkdir("workspace")
	f.writeDefaultConfig(foldersConfiguration(workspace))
	before, err := os.ReadFile(f.defaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.command.loadState()
	if err != nil {
		t.Fatal(err)
	}
	next := foldersReal(t, f.mkdir("next"))
	entry := folderEntry{Folder: next, Config: perFolderConfig(state.options, next), State: perFolderState(state.options, next)}
	if err = f.command.createFolderConfig(t.Context(), state, entry); err != nil {
		t.Fatal(err)
	}

	// The folder's runtime comes from the injected first-run build.
	if len(f.runtimes) != 1 {
		t.Fatalf("claude runtime built %d times, want 1", len(f.runtimes))
	}
	options := f.runtimes[0]
	if options.config != entry.Config || options.state != entry.State || options.sessions != filepath.Join(entry.State, "sessions") ||
		options.defaultCredentials != filepath.Join(state.options.state, "credentials") ||
		options.subagentModel != filepath.Join(state.options.state, "subagent-model.json") {
		t.Fatalf("folder options = %+v", options)
	}

	created, err := os.ReadFile(entry.Config)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(entry.Config); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("folder configuration = %v, %v", info, err)
	}
	recordPath := filepath.Join(entry.State, folderRecordName)
	if info, err := os.Stat(recordPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("folder record = %v, %v", info, err)
	}
	if info, err := os.Stat(entry.State); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("folder state = %v, %v", info, err)
	}
	if record, err := readFolderRecord(entry.State); err != nil || record.Version != 1 || record.Workspace != next {
		t.Fatalf("folder record = %+v, %v", record, err)
	}

	written, err := readServeConfiguration(entry.Config)
	if err != nil {
		t.Fatal(err)
	}
	base := state.config
	if written.Runtime.Version != base.Runtime.Version || written.Runtime.SystemPrompt != base.Runtime.SystemPrompt ||
		!slices.Equal(written.Runtime.DisallowedTools, base.Runtime.DisallowedTools) {
		t.Fatalf("identity fields changed: %+v", written.Runtime)
	}
	if written.Runtime.Provider.Provider != claudeagent.ProviderID || written.Runtime.Provider.Model.ID != f.identity.Provider.Model.ID ||
		!reflect.DeepEqual(written.Runtime.Profile, f.identity.Profile) || written.Runtime.ReasoningEffort != f.identity.ReasoningEffort ||
		written.Runtime.Claude == nil || written.Runtime.Claude.Binary != f.identity.Claude.Binary {
		t.Fatalf("runtime = %+v", written.Runtime)
	}
	if written.Runtime.Workspace != next {
		t.Fatalf("workspace = %q, want %q", written.Runtime.Workspace, next)
	}
	if want := []string{next, "/shared/data", next}; !slices.Equal(written.Permissions.ReadRoots, want) {
		t.Fatalf("read roots = %q, want %q", written.Permissions.ReadRoots, want)
	}
	if want := []string{next, "/shared/out"}; !slices.Equal(written.Permissions.WriteRoots, want) {
		t.Fatalf("write roots = %q, want %q", written.Permissions.WriteRoots, want)
	}
	if !reflect.DeepEqual(written.PermissionModes, base.PermissionModes) {
		t.Fatalf("permission modes changed: %+v", written.PermissionModes)
	}
	if !slices.Equal(written.Permissions.Tools, base.Permissions.Tools) ||
		!slices.Equal(written.Permissions.NetworkOrigins, base.Permissions.NetworkOrigins) ||
		written.Permissions.ProcessMode != base.Permissions.ProcessMode {
		t.Fatalf("permissions changed: %+v", written.Permissions)
	}
	if written.LanguageServers[0].Path != next || written.LanguageServers[0].Arguments[0] != next ||
		written.Subagents["lead"].Runtime.Workspace != next || written.Subagents["lead"].Workers["worker"].Baseline.ReadRoots[0] != next {
		t.Fatalf("configuration fields were not relocated: %+v", written)
	}

	// The default configuration is read only; a second creation keeps the
	// existing folder bytes and does not build another runtime.
	after, err := os.ReadFile(f.defaultConfigPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("default runtime.json changed: %v", err)
	}
	if err = f.command.createFolderConfig(t.Context(), state, entry); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(entry.Config)
	if err != nil || !bytes.Equal(created, again) || len(f.runtimes) != 1 {
		t.Fatalf("existing folder configuration rewritten: %v, builds %d", err, len(f.runtimes))
	}

	// The folder record is written for an existing configuration too, so one
	// open writes it exactly once whichever branch it takes.
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err = f.command.createFolderConfig(t.Context(), state, entry); err != nil {
		t.Fatal(err)
	}
	if record, err := readFolderRecord(entry.State); err != nil || record.Version != 1 || record.Workspace != next {
		t.Fatalf("folder record = %+v, %v", record, err)
	}
	if again, err = os.ReadFile(entry.Config); err != nil || !bytes.Equal(created, again) || len(f.runtimes) != 1 {
		t.Fatalf("existing folder configuration rewritten: %v, builds %d", err, len(f.runtimes))
	}
}

// TestFoldersCreateConfigurationFirstOpenFailure is a new folder whose first
// open fails: no configuration is published and no state directory or folder
// record is left behind, so the folder stays unknown.
func TestFoldersCreateConfigurationFirstOpenFailure(t *testing.T) {
	f := newFoldersFixture(t)
	workspace := f.mkdir("workspace")
	f.writeDefaultConfig(foldersConfiguration(workspace))
	state, err := f.command.loadState()
	if err != nil {
		t.Fatal(err)
	}
	next := foldersReal(t, f.mkdir("next"))
	entry := folderEntry{Folder: next, Config: perFolderConfig(state.options, next), State: perFolderState(state.options, next)}
	failure := errors.New("initial_provider_unavailable")
	f.command.claudeRuntime = func(context.Context, launchOptions) (agentrunner.RuntimeIdentity, error) {
		return agentrunner.RuntimeIdentity{}, failure
	}
	if err := f.command.createFolderConfig(t.Context(), state, entry); !errors.Is(err, failure) {
		t.Fatalf("createFolderConfig = %v, want %v", err, failure)
	}
	if _, err := os.Lstat(entry.Config); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("folder configuration = %v, want none", err)
	}
	if _, err := os.Lstat(entry.State); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("folder state = %v, want none", err)
	}
}

// TestFoldersClaudeRuntimePinnedThroughBootstrap runs the real bootstrap: a
// folder never starts on the login-required runtime or on another provider when
// Claude is not registered.
func TestFoldersClaudeRuntimePinnedThroughBootstrap(t *testing.T) {
	options := func(t *testing.T) launchOptions {
		t.Helper()
		parsed, err := parseLaunchOptions(nil, io.Discard, os.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	for _, scenario := range []struct {
		name  string
		codex bool
	}{
		{"no provider", false},
		{"another provider only", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			normalFixture(t, scenario.codex, false)
			command := foldersCommand{getenv: os.Getenv}
			_, err := command.discoverClaudeRuntime(t.Context(), options(t))
			var failure *normalRuntimeError
			if !errors.As(err, &failure) || failure.Code != "initial_provider_unavailable" || failure.Provider != claudeagent.ProviderID {
				t.Fatalf("claude runtime = %v, want initial_provider_unavailable for %s", err, claudeagent.ProviderID)
			}
		})
	}

	t.Run("claude registered", func(t *testing.T) {
		normalFixture(t, false, true)
		command := foldersCommand{getenv: os.Getenv}
		identity, err := command.discoverClaudeRuntime(t.Context(), options(t))
		if err != nil || identity.Provider.Provider != claudeagent.ProviderID {
			t.Fatalf("claude runtime = %+v, %v", identity, err)
		}
	})
}

func TestFoldersOpenByName(t *testing.T) {
	t.Run("one", func(t *testing.T) {
		f := newFoldersFixture(t)
		f.writeDefaultConfig(foldersConfiguration(f.mkdir("workspace")))
		other := f.addFolder("other", map[string]time.Time{"named": time.Now().Add(-time.Hour)})
		if err := f.run("named"); err != nil {
			t.Fatal(err)
		}
		if len(f.launches) != 1 || len(f.chdirs) != 1 || f.chdirs[0] != other.Folder {
			t.Fatalf("launches = %q, chdirs = %q", f.launches, f.chdirs)
		}
		want := []string{"--config", other.Config, "--state-directory", other.State, "--shared-state-directory", f.defaultState(), "named"}
		if !slices.Equal(f.launches[0], want) {
			t.Fatalf("launch = %q, want %q", f.launches[0], want)
		}
		if _, err := os.Stat(other.Config); err != nil {
			t.Fatalf("folder configuration missing: %v", err)
		}
	})

	t.Run("several", func(t *testing.T) {
		f := newFoldersFixture(t)
		f.writeDefaultConfig(foldersConfiguration(f.mkdir("workspace")))
		f.writeSessions(f.defaultState(), map[string]time.Time{"named": time.Now().Add(-2 * time.Hour)})
		other := f.addFolder("other", map[string]time.Time{"named": time.Now().Add(-time.Hour)})
		f.stdin("2\n")
		if err := f.run("named"); err != nil {
			t.Fatal(err)
		}
		// The default folder is the first known folder, so 2 is the other one.
		want := []string{"--config", other.Config, "--state-directory", other.State, "--shared-state-directory", f.defaultState(), "named"}
		if len(f.launches) != 1 || !slices.Equal(f.launches[0], want) || len(f.chdirs) != 1 || f.chdirs[0] != other.Folder {
			t.Fatalf("launches = %q, chdirs = %q, want %q", f.launches, f.chdirs, want)
		}
		if lines := f.listedLines(); len(lines) != 2 ||
			!strings.HasPrefix(lines[0], "1) ~/workspace  ") || !strings.HasPrefix(lines[1], "2) ~/other  ") {
			t.Fatalf("folder choice = %q", lines)
		}
	})

	t.Run("none opens the current directory", func(t *testing.T) {
		f := newFoldersFixture(t)
		f.writeDefaultConfig(foldersConfiguration(f.mkdir("workspace")))
		current := foldersReal(t, f.mkdir("current"))
		t.Chdir(current)
		if err := f.run("fresh"); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(f.home, ".config", "unreal-agent", foldersDirectoryName, folderID(current), folderConfigName)
		state := filepath.Join(f.home, ".local", "state", "unreal-agent", foldersDirectoryName, folderID(current))
		want := []string{"--config", config, "--state-directory", state, "--shared-state-directory", f.defaultState(), "fresh"}
		if len(f.launches) != 1 || !slices.Equal(f.launches[0], want) || len(f.chdirs) != 1 || f.chdirs[0] != current {
			t.Fatalf("launches = %q, chdirs = %q, want %q", f.launches, f.chdirs, want)
		}
		if record, err := readFolderRecord(state); err != nil || record.Workspace != current {
			t.Fatalf("folder record = %+v, %v", record, err)
		}
		if _, err := os.Stat(config); err != nil {
			t.Fatalf("folder configuration missing: %v", err)
		}
	})
}

func TestFoldersList(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	f := newFoldersFixture(t)
	workspace := foldersReal(t, f.mkdir("workspace"))
	f.writeDefaultConfig(foldersConfiguration(workspace))
	f.writeSessions(f.defaultState(), map[string]time.Time{
		"old":          now.Add(-3 * time.Hour),
		"new":          now.Add(-2 * time.Hour),
		"child-worker": now.Add(-time.Minute),
	})
	other := f.addFolder("other", map[string]time.Time{"other": now.Add(-time.Hour)})

	f.stdin("\n")
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if len(f.launches) != 0 || len(f.chdirs) != 0 {
		t.Fatalf("an empty line launched something: %q %q", f.launches, f.chdirs)
	}
	lines := f.listedLines()
	want := []string{"1) other  ~/other  ", "2) new  ~/workspace  ", "3) old  ~/workspace  "}
	if len(lines) != len(want) {
		t.Fatalf("list = %q, want %d lines", lines, len(want))
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d = %q, want prefix %q", i+1, lines[i], prefix)
		}
	}
	if strings.Contains(f.output.String(), folderChildPrefix+"worker") {
		t.Fatal("a child Session was listed")
	}

	// The newest Session belongs to the other folder, so picking it launches
	// that folder with its own configuration and state.
	f.stdin("1\n")
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", other.Config, "--state-directory", other.State, "--shared-state-directory", f.defaultState(), "other"}
	if len(f.launches) != 1 || !slices.Equal(f.launches[0], args) || len(f.chdirs) != 1 || f.chdirs[0] != other.Folder {
		t.Fatalf("launches = %q, chdirs = %q, want %q", f.launches, f.chdirs, args)
	}

	for _, input := range []string{"9\n", "zero\n"} {
		f.stdin(input)
		if err := f.run(); err == nil {
			t.Fatalf("%q was accepted", input)
		}
	}
}

func TestFoldersWhere(t *testing.T) {
	f := newFoldersFixture(t)
	f.writeDefaultConfig(foldersConfiguration(f.mkdir("workspace")))
	first := f.addFolder("first", map[string]time.Time{"named": time.Now()})
	second := f.addFolder("second", map[string]time.Time{"named": time.Now()})

	if err := f.run("--where", "named"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(f.output.String()); len(got) != 2 || !slices.Contains(got, first.Folder) || !slices.Contains(got, second.Folder) {
		t.Fatalf("where = %q", f.output.String())
	}
	if len(f.launches) != 0 {
		t.Fatalf("where launched %q", f.launches)
	}

	f = newFoldersFixture(t)
	f.writeDefaultConfig(foldersConfiguration(f.mkdir("workspace")))
	only := f.addFolder("only", map[string]time.Time{"named": time.Now()})
	if err := f.run("--where", "named"); err != nil || strings.TrimSpace(f.output.String()) != only.Folder {
		t.Fatalf("where = %q, %v", f.output.String(), err)
	}
	if err := f.run("--where", "missing"); err == nil {
		t.Fatal("a missing name was accepted")
	}
}

func TestFoldersDefaultFolderLaunch(t *testing.T) {
	f := newFoldersFixture(t)
	workspace := foldersReal(t, f.mkdir("workspace"))
	f.writeDefaultConfig(foldersConfiguration(workspace))
	f.writeSessions(f.defaultState(), map[string]time.Time{"mine": time.Now()})
	if err := f.run("mine"); err != nil {
		t.Fatal(err)
	}
	if len(f.launches) != 1 || !slices.Equal(f.launches[0], []string{"mine"}) || len(f.chdirs) != 1 || f.chdirs[0] != workspace {
		t.Fatalf("launches = %q, chdirs = %q", f.launches, f.chdirs)
	}
	if len(f.runtimes) != 0 {
		t.Fatalf("the default folder built a runtime: %+v", f.runtimes)
	}
}

func TestFoldersUsage(t *testing.T) {
	f := newFoldersFixture(t)
	for _, args := range [][]string{{"-h"}, {"--help"}} {
		f.output.Reset()
		if err := f.run(args...); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"folders", "--where", "NAME"} {
			if !strings.Contains(f.output.String(), want) {
				t.Fatalf("usage for %q lacks %q: %s", args, want, f.output.String())
			}
		}
	}
	if len(f.launches) != 0 {
		t.Fatalf("usage launched %q", f.launches)
	}
	if err := f.run("one", "two"); err == nil {
		t.Fatal("two names were accepted")
	}
	if err := f.run("--where", "one", "two"); err == nil {
		t.Fatal("a name with --where was accepted")
	}
}

func TestFoldersMissingDefaultConfiguration(t *testing.T) {
	f := newFoldersFixture(t)
	if err := f.run(); err == nil {
		t.Fatal("a missing default configuration was accepted")
	}
	f.writeDefaultConfig(serveConfiguration{Runtime: agentrunner.RuntimeIdentity{Version: 1}})
	if err := f.run("--where", "name"); err == nil {
		t.Fatal("an empty workspace was accepted")
	}
	if len(f.launches) != 0 {
		t.Fatalf("launched %q", f.launches)
	}
}
