package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/dap"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/lsp"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/internal/privateexport"
)

// The folders command opens one workspace's conversation from every folder the
// default configuration knows. Each folder keeps its own configuration and
// state under the existing roots; the login and the /model-sub choice stay in
// the default state directory, so one folder never needs its own login.

const (
	// folderTimeLayout is the fixed local-time format of every listing, so the
	// output shape is deterministic for people and tests.
	folderTimeLayout = "2006-01-02 15:04"
	// foldersDirectoryName holds one entry per folder, named by its folder ID.
	foldersDirectoryName = "folders"
	folderRecordName     = "folder.json"
	folderConfigName     = "runtime.json"
	// folderChildPrefix marks the Session names of child agents; they are not
	// conversations a person opens from the list.
	folderChildPrefix = "child-"
)

// folderRecord is STATE/folder.json: which workspace a state directory belongs
// to. Version 1 is the only shape this command writes.
type folderRecord struct {
	Version   int
	Workspace string
}

// folderEntry is one known workspace and where its configuration and state live.
type folderEntry struct {
	Folder, Config, State string
}

// folderSession is one Session in one known folder.
type folderSession struct {
	entry folderEntry
	info  sessionstore.SessionInfo
}

// folderState is what one invocation resolves before it acts: the default
// launcher options and configuration, the default folder, and every known
// folder.
type folderState struct {
	options       launchOptions
	config        serveConfiguration
	defaultFolder string
	known         []folderEntry
}

// foldersCommand keeps the seams the command needs, so tests run without a
// Host, discovery or a network call. Production values are set by
// newFoldersCommand; a test replaces the seams it must not exercise.
type foldersCommand struct {
	input  io.Reader
	output io.Writer
	getenv func(string) string
	// launch runs the launcher in process and returns when the conversation ends.
	launch func(context.Context, []string, io.Writer) error
	chdir  func(string) error
	// claudeRuntime builds a folder's runtime identity. The default reuses the
	// existing first-run discovery in bootstrapRuntime.
	claudeRuntime func(context.Context, launchOptions) (agentrunner.RuntimeIdentity, error)
}

func newFoldersCommand() foldersCommand {
	c := foldersCommand{input: os.Stdin, output: os.Stdout, getenv: os.Getenv, launch: runLauncher, chdir: os.Chdir}
	c.claudeRuntime = c.discoverClaudeRuntime
	return c
}

// runFolders is `unreal-agent folders [NAME | --where NAME | -h]`.
func runFolders(ctx context.Context, args []string, output io.Writer) error {
	c := newFoldersCommand()
	c.output = output
	return c.run(ctx, args)
}

func (c foldersCommand) run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("folders", flag.ContinueOnError)
	flags.SetOutput(c.output)
	where := flags.String("where", "", "NAME の会話があるフォルダを表示します")
	help := flags.Bool("h", false, "この使い方を表示します")
	flags.Usage = func() {
		fmt.Fprintln(c.output, strings.Join([]string{
			"使い方: unreal-agent folders [NAME | --where NAME | -h]",
			"  folders NAME          NAME の会話を開きます。別のフォルダにもあれば番号で選びます",
			"  folders --where NAME  NAME の会話があるフォルダを表示します",
			"  folders               すべてのフォルダの会話から番号で選んで開きます",
			"  -h, --help            この使い方を表示します",
		}, "\n"))
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed the same usage
		}
		return err
	}
	if *help {
		flags.Usage()
		return nil
	}
	if flags.NArg() > 1 {
		return errors.New("使い方: unreal-agent folders [NAME | --where NAME | -h]")
	}
	if *where != "" && flags.NArg() != 0 {
		return errors.New("NAME と --where は同時に指定できません。")
	}
	state, err := c.loadState()
	if err != nil {
		return err
	}
	if *where != "" {
		return c.where(ctx, state, session.ID(*where))
	}
	if flags.NArg() == 1 {
		return c.open(ctx, state, session.ID(flags.Arg(0)))
	}
	return c.list(ctx, state)
}

// loadState resolves the default configuration and every known folder. Without
// the default runtime.json the command cannot know any folder, so it asks for
// the one real start first.
func (c foldersCommand) loadState() (folderState, error) {
	options, err := parseLaunchOptions(nil, io.Discard, c.getenv)
	if err != nil {
		return folderState{}, err
	}
	config, err := readServeConfiguration(options.config)
	if err != nil || config.Runtime.Workspace == "" {
		return folderState{}, errors.New("既定の設定を読み取れません。まず unreal を一度起動してから、もう一度実行してください。")
	}
	folder, err := folderFor(config.Runtime.Workspace)
	if err != nil {
		return folderState{}, err
	}
	known, err := knownFolders(options, folder)
	if err != nil {
		return folderState{}, err
	}
	return folderState{options: options, config: config, defaultFolder: folder, known: known}, nil
}

// folderFor is the folder a directory belongs to: its git top level when it has
// one, otherwise the directory itself. Symlinks are resolved so one workspace
// has one identity (macOS /var -> /private/var).
func folderFor(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if out, err := exec.Command("git", "-C", absolute, "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			if absolute, err = filepath.Abs(top); err != nil {
				return "", err
			}
		}
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	}
	return filepath.Clean(absolute), nil
}

// folderID names one workspace's configuration and state.
// scripts/common_development.py computes the same key from the same bytes.
func folderID(folder string) string {
	sum := sha256.Sum256([]byte(folder))
	return "r-" + hex.EncodeToString(sum[:])[:16]
}

// perFolderConfig and perFolderState name one folder's storage beside the
// default configuration and state: <config root>/folders/<ID>/runtime.json and
// <state root>/folders/<ID>/. The config root is the directory holding the
// default runtime.json; the state root is the default state directory itself,
// so both stay inside this installation's configuration and state.
func perFolderConfig(options launchOptions, folder string) string {
	return filepath.Join(filepath.Dir(options.config), foldersDirectoryName, folderID(folder), folderConfigName)
}

func perFolderState(options launchOptions, folder string) string {
	return filepath.Join(options.state, foldersDirectoryName, folderID(folder))
}

// knownFolders is the default folder plus every per-folder entry. An entry
// without a readable folder.json is skipped: its workspace is unknown.
func knownFolders(options launchOptions, defaultFolder string) ([]folderEntry, error) {
	known := []folderEntry{{Folder: defaultFolder, Config: options.config, State: options.state}}
	directories, err := os.ReadDir(filepath.Join(filepath.Dir(options.config), foldersDirectoryName))
	if errors.Is(err, fs.ErrNotExist) {
		return known, nil
	}
	if err != nil {
		return nil, err
	}
	for _, directory := range directories {
		if !directory.IsDir() {
			continue
		}
		stored := folderEntry{
			Config: filepath.Join(filepath.Dir(options.config), foldersDirectoryName, directory.Name(), folderConfigName),
			State:  filepath.Join(options.state, foldersDirectoryName, directory.Name()),
		}
		record, err := readFolderRecord(stored.State)
		if err != nil || record.Workspace == "" {
			continue
		}
		stored.Folder = record.Workspace
		known = append(known, stored)
	}
	return known, nil
}

func readFolderRecord(state string) (folderRecord, error) {
	var record folderRecord
	data, err := os.ReadFile(filepath.Join(state, folderRecordName))
	if err != nil {
		return folderRecord{}, err
	}
	if json.Unmarshal(data, &record, json.RejectUnknownMembers(true)) != nil || record.Version != 1 {
		return folderRecord{}, errors.New("invalid folder record")
	}
	return record, nil
}

// writeFolderRecord replaces STATE/folder.json whole, so a reader never sees a
// partial record. The state directory is private, as every launcher state is.
func writeFolderRecord(state, folder string) error {
	if err := makePrivateDirectory(state); err != nil {
		return err
	}
	data, err := json.Marshal(folderRecord{Version: 1, Workspace: folder})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(state, ".folder-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(state, folderRecordName))
}

// discoverClaudeRuntime is the default claudeRuntime: the folder's first-run
// discovery, always on Claude, with the existing bootstrap so there is no
// second provider or model table. A folder always runs Claude, so the bootstrap
// provider is pinned to Claude and no chooser can run: when Claude is not
// registered the bootstrap fails with initial_provider_unavailable (Provider
// Claude) instead of writing a login-required or another provider's runtime.
func (c foldersCommand) discoverClaudeRuntime(ctx context.Context, options launchOptions) (agentrunner.RuntimeIdentity, error) {
	getenv := func(key string) string {
		// A folder always runs Claude, so the environment cannot pin a model for
		// it, and its provider is the pinned Claude below.
		switch key {
		case "UNREAL_HARNESS_LLM_PROVIDER":
			return claudeagent.ProviderID
		case "UNREAL_HARNESS_LLM_MODEL":
			return ""
		}
		return c.getenv(key)
	}
	config, err := bootstrapRuntime(ctx, options, nil, getenv)
	if err != nil {
		return agentrunner.RuntimeIdentity{}, err
	}
	// Only the identity is used; bootstrap's permissions stay the default
	// folder's.
	return config.Runtime, nil
}

// createFolderConfig publishes a folder's configuration once, copied from the
// default one, and only when the folder has none. An existing file is never
// edited. The folder record is written exactly once per open: before the
// configuration is published, or alone when the configuration already exists.
func (c foldersCommand) createFolderConfig(ctx context.Context, state folderState, entry folderEntry) error {
	if _, err := os.Lstat(entry.Config); err == nil {
		return writeFolderRecord(entry.State, entry.Folder)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	options, err := parseLaunchOptions([]string{"--config", entry.Config, "--state-directory", entry.State, "--shared-state-directory", state.options.state}, io.Discard, c.getenv)
	if err != nil {
		return err
	}
	identity, err := c.claudeRuntime(ctx, options)
	if err != nil {
		return err
	}
	config := relocateConfiguration(state.config, state.config.Runtime.Workspace, entry.Folder)
	config.Runtime.Provider = identity.Provider
	config.Runtime.Profile = identity.Profile
	config.Runtime.ReasoningEffort = identity.ReasoningEffort
	config.Runtime.Claude = identity.Claude
	if err = makePrivateDirectory(entry.State); err != nil {
		return err
	}
	if err = writeFolderRecord(entry.State, entry.Folder); err != nil {
		return err
	}
	data, err := json.Marshal(config, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	// Another writer may have published a configuration meanwhile. Its file
	// stays untouched and the launcher uses it as it is.
	if err = privateexport.CreateRuntimeConfig(ctx, entry.Config, append(data, '\n')); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// open resolves NAME. One folder opens it; several ask which one, with that
// Session's last used time; none opens NAME in the current directory's folder.
func (c foldersCommand) open(ctx context.Context, state folderState, name session.ID) error {
	found, err := c.findSessions(ctx, state.known, name)
	if err != nil {
		return err
	}
	switch len(found) {
	case 1:
		return c.openFolder(ctx, state, found[0].entry, name)
	case 0:
		folder, err := folderFor(".")
		if err != nil {
			return err
		}
		return c.openFolder(ctx, state, folderEntry{Folder: folder, Config: perFolderConfig(state.options, folder), State: perFolderState(state.options, folder)}, name)
	}
	home := c.getenv("HOME")
	for i, match := range found {
		fmt.Fprintf(c.output, "%d) %s  %s\n", i+1, displayFolder(match.entry.Folder, home), match.info.LastUpdatedAt.Local().Format(folderTimeLayout))
	}
	fmt.Fprint(c.output, "番号を選んでください: ")
	choice, err := c.choose(len(found))
	if err != nil {
		return err
	}
	if choice == 0 {
		return errors.New("フォルダを選ぶ番号を入力してください。")
	}
	return c.openFolder(ctx, state, found[choice-1].entry, name)
}

// openFolder makes a folder usable and launches its conversation in process.
// The default folder starts exactly as a plain unreal start does; another
// folder names its own configuration and state and shares the default state
// for the login and the /model-sub choice.
func (c foldersCommand) openFolder(ctx context.Context, state folderState, entry folderEntry, name session.ID) error {
	arguments := []string{string(name)}
	if entry.Folder != state.defaultFolder {
		// createFolderConfig writes the folder record once per open.
		if err := c.createFolderConfig(ctx, state, entry); err != nil {
			return err
		}
		arguments = []string{"--config", entry.Config, "--state-directory", entry.State, "--shared-state-directory", state.options.state, string(name)}
	}
	if err := c.chdir(entry.Folder); err != nil {
		return fmt.Errorf("%s に移動できません: %w", entry.Folder, err)
	}
	return c.launch(ctx, arguments, c.output)
}

// list shows every Session of every known folder, newest last-used first, and
// opens the one the user picks from the folder it belongs to.
func (c foldersCommand) list(ctx context.Context, state folderState) error {
	var listed []folderSession
	for _, entry := range state.known {
		sessions, err := c.folderSessions(ctx, entry)
		if err != nil {
			return err
		}
		for _, info := range sessions {
			if strings.HasPrefix(string(info.ID), folderChildPrefix) {
				continue // a child agent is not a conversation to open
			}
			listed = append(listed, folderSession{entry: entry, info: info})
		}
	}
	if len(listed) == 0 {
		fmt.Fprintln(c.output, "会話がありません。")
		return nil
	}
	slices.SortStableFunc(listed, func(left, right folderSession) int {
		if !left.info.LastUpdatedAt.Equal(right.info.LastUpdatedAt) {
			return right.info.LastUpdatedAt.Compare(left.info.LastUpdatedAt)
		}
		if left.info.ID != right.info.ID {
			return strings.Compare(string(left.info.ID), string(right.info.ID))
		}
		return strings.Compare(left.entry.Folder, right.entry.Folder)
	})
	home := c.getenv("HOME")
	for i, item := range listed {
		fmt.Fprintf(c.output, "%d) %s  %s  %s\n", i+1, item.info.ID, displayFolder(item.entry.Folder, home), item.info.LastUpdatedAt.Local().Format(folderTimeLayout))
	}
	fmt.Fprint(c.output, "番号を選んでください（Enter で終了）: ")
	choice, err := c.choose(len(listed))
	if err != nil {
		return err
	}
	if choice == 0 {
		return nil
	}
	chosen := listed[choice-1]
	return c.openFolder(ctx, state, chosen.entry, chosen.info.ID)
}

// where prints the folder of every known folder that has NAME.
func (c foldersCommand) where(ctx context.Context, state folderState, name session.ID) error {
	found, err := c.findSessions(ctx, state.known, name)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("%s の会話は、どのフォルダにもありません。", name)
	}
	for _, match := range found {
		fmt.Fprintln(c.output, match.entry.Folder)
	}
	return nil
}

func (c foldersCommand) findSessions(ctx context.Context, known []folderEntry, name session.ID) ([]folderSession, error) {
	var found []folderSession
	for _, entry := range known {
		sessions, err := c.folderSessions(ctx, entry)
		if err != nil {
			return nil, err
		}
		for _, info := range sessions {
			if info.ID == name {
				found = append(found, folderSession{entry: entry, info: info})
			}
		}
	}
	return found, nil
}

// folderSessions reads STATE/sessions metadata. A missing directory means no
// Sessions; this command never creates it.
func (c foldersCommand) folderSessions(ctx context.Context, entry folderEntry) ([]sessionstore.SessionInfo, error) {
	directory := filepath.Join(entry.State, "sessions")
	if _, err := os.Stat(directory); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	store, err := localfile.New(directory)
	if err != nil {
		return nil, err
	}
	return store.ListSessions(ctx)
}

// choose reads one 1-based number from the input; 0 means the line was empty.
func (c foldersCommand) choose(limit int) (int, error) {
	line, err := bufio.NewReader(c.input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, nil
	}
	number, err := strconv.Atoi(line)
	if err != nil {
		return 0, errors.New("番号を入力してください。")
	}
	if number < 1 || number > limit {
		return 0, fmt.Errorf("1 から %d の番号を入力してください。", limit)
	}
	return number, nil
}

// displayFolder shortens HOME to ~ in listings. The folder itself is unchanged.
func displayFolder(folder, home string) string {
	if home == "" || !strings.HasPrefix(folder, home+string(filepath.Separator)) {
		return folder
	}
	return "~" + strings.TrimPrefix(folder, home)
}

// relocateConfiguration copies a folder's configuration from the default one.
// Only a field that holds the old workspace path by exact string equality names
// the folder instead; nothing else about the operator's policy is inferred,
// guessed or rewritten.
func relocateConfiguration(config serveConfiguration, oldWorkspace, folder string) serveConfiguration {
	config.Runtime.Workspace = folder
	config.Permissions = relocatePermissions(config.Permissions, oldWorkspace, folder)
	if config.PermissionModes != nil {
		modes := *config.PermissionModes
		if modes.Maximum != nil {
			maximum := relocatePermissions(*modes.Maximum, oldWorkspace, folder)
			modes.Maximum = &maximum
		}
		config.PermissionModes = &modes
	}
	if config.LanguageServers != nil {
		servers := make([]lsp.ServerConfig, len(config.LanguageServers))
		for i, server := range config.LanguageServers {
			server.Path = relocatePath(server.Path, oldWorkspace, folder)
			server.Arguments = relocatePaths(server.Arguments, oldWorkspace, folder)
			server.Environment = relocatePaths(server.Environment, oldWorkspace, folder)
			servers[i] = server
		}
		config.LanguageServers = servers
	}
	if config.DebugAdapters != nil {
		adapters := make([]dap.AdapterConfig, len(config.DebugAdapters))
		for i, adapter := range config.DebugAdapters {
			adapter.Path = relocatePath(adapter.Path, oldWorkspace, folder)
			adapter.Directory = relocatePath(adapter.Directory, oldWorkspace, folder)
			adapter.Arguments = relocatePaths(adapter.Arguments, oldWorkspace, folder)
			adapter.Environment = relocatePaths(adapter.Environment, oldWorkspace, folder)
			adapters[i] = adapter
		}
		config.DebugAdapters = adapters
	}
	if config.Subagents != nil {
		subagents := make(map[string]childTemplate, len(config.Subagents))
		for name, template := range config.Subagents {
			subagents[name] = relocateTemplate(template, oldWorkspace, folder)
		}
		config.Subagents = subagents
	}
	// Launcher and Providers name no workspace path: they are kept exactly as
	// the default configuration has them.
	return config
}

func relocateTemplate(template childTemplate, oldWorkspace, folder string) childTemplate {
	template.Runtime.Workspace = relocatePath(template.Runtime.Workspace, oldWorkspace, folder)
	template.Permissions = relocatePermissions(template.Permissions, oldWorkspace, folder)
	if template.Baseline != nil {
		baseline := relocatePermissions(*template.Baseline, oldWorkspace, folder)
		template.Baseline = &baseline
	}
	if template.Workers != nil {
		workers := make(map[string]childTemplate, len(template.Workers))
		for name, worker := range template.Workers {
			workers[name] = relocateTemplate(worker, oldWorkspace, folder)
		}
		template.Workers = workers
	}
	return template
}

func relocatePermissions(config permission.Config, oldWorkspace, folder string) permission.Config {
	config.ReadRoots = relocatePaths(config.ReadRoots, oldWorkspace, folder)
	config.WriteRoots = relocatePaths(config.WriteRoots, oldWorkspace, folder)
	return config
}

func relocatePath(value, oldWorkspace, folder string) string {
	if value == "" || value != oldWorkspace {
		return value
	}
	return folder
}

// relocatePaths keeps order and duplicates; a nil slice stays nil.
func relocatePaths(values []string, oldWorkspace, folder string) []string {
	if values == nil {
		return nil
	}
	relocated := make([]string, len(values))
	for i, value := range values {
		relocated[i] = relocatePath(value, oldWorkspace, folder)
	}
	return relocated
}
