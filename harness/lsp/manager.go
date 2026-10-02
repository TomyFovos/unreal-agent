package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

type document struct {
	snapshot mutation.Snapshot
	version  int
}
type preparedAction struct {
	generation string
	edit       workspaceEdit
	snapshots  map[string]mutation.Snapshot
	versions   map[string]int
}
type instance struct {
	mu            sync.Mutex
	config        ServerConfig
	generation    string
	rpc           *connection
	encoding      string
	syncOpenClose bool
	syncChange    int
	capabilities  map[string]json.RawMessage
	documents     map[string]document
	actions       map[string]preparedAction
}
type Manager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mutation *mutation.Service
	timeout  time.Duration
	mu       sync.Mutex
	configs  map[string]ServerConfig
	servers  map[string]*instance
	closed   bool
}

// New creates a workspace-scoped resource owner. Close is called when its Host
// lifetime ends. Session adapters borrow this Manager; they do not own processes.
func New(ctx context.Context, config Config) (*Manager, error) {
	if config.Mutation == nil {
		return nil, errors.New("LSP requires a mutation service")
	}
	if len(config.Servers) > 32 {
		return nil, errors.New("too many language servers")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("invalid request timeout")
	}
	life, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: life, cancel: cancel, mutation: config.Mutation, timeout: config.RequestTimeout, configs: make(map[string]ServerConfig), servers: make(map[string]*instance)}
	for _, server := range config.Servers {
		if server.Language == "" || !filepath.IsAbs(server.Path) {
			cancel()
			return nil, errors.New("server requires language and absolute executable")
		}
		if _, exists := m.configs[server.Language]; exists {
			cancel()
			return nil, errors.New("duplicate server language")
		}
		server.Arguments = append([]string(nil), server.Arguments...)
		server.Environment = append([]string(nil), server.Environment...)
		server.Extensions = append([]string(nil), server.Extensions...)
		m.configs[server.Language] = server
	}
	return m, nil
}
func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}
func (m *Manager) path(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", failure("unsupported", "only local workspace file URIs are supported")
	}
	path, err := m.mutation.Path(filepath.FromSlash(u.Path))
	if err != nil {
		return "", failure("invalid", "edit target is outside workspace")
	}
	rel, _ := filepath.Rel(m.mutation.Root(), path)
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" {
			return "", failure("denied", "version-control metadata is not an edit target")
		}
	}
	return path, nil
}
func (m *Manager) get(ctx context.Context, language, generation string) (*instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return nil, failure("expired", "language-server owner is closed")
	}
	if server := m.servers[language]; server != nil && server.rpc.live() {
		if generation != "" && generation != server.generation {
			return nil, failure("expired", "language-server generation is stale")
		}
		return server, nil
	}
	if generation != "" {
		return nil, failure("expired", "language-server generation is stale")
	}
	config, exists := m.configs[language]
	if !exists {
		return nil, failure("unavailable", "no server configured for language")
	}
	rpc, err := startConnection(m.ctx, config, m.mutation.Root())
	if err != nil {
		return nil, err
	}
	server := &instance{config: config, generation: uuid.New().String(), rpc: rpc, encoding: "utf-16", documents: make(map[string]document), actions: make(map[string]preparedAction)}
	data, err := rpc.request(ctx, "initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": fileURI(m.mutation.Root()), "workspaceFolders": []any{map[string]any{"uri": fileURI(m.mutation.Root()), "name": filepath.Base(m.mutation.Root())}},
		"capabilities": map[string]any{
			"general":      map[string]any{"positionEncodings": []string{"utf-16", "utf-8", "utf-32"}},
			"workspace":    map[string]any{"applyEdit": false, "workspaceEdit": map[string]any{"documentChanges": true, "resourceOperations": []string{"create"}}},
			"textDocument": map[string]any{"synchronization": map[string]any{"dynamicRegistration": false}, "publishDiagnostics": map[string]any{"versionSupport": true}, "diagnostic": map[string]any{"dynamicRegistration": false}, "codeAction": map[string]any{"codeActionLiteralSupport": map[string]any{"codeActionKind": map[string]any{"valueSet": []string{"quickfix", "refactor", "source"}}}}},
		},
	})
	if err == nil {
		var response struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		}
		if json.Unmarshal(data, &response) != nil || response.Capabilities == nil {
			err = failure("protocol", "initialize response lacks capabilities")
		} else {
			server.capabilities = response.Capabilities
		}
	}
	if err == nil {
		raw := server.capabilities["textDocumentSync"]
		var kind int
		if json.Unmarshal(raw, &kind) == nil {
			server.syncOpenClose = kind == 1 || kind == 2
			server.syncChange = kind
		} else {
			var options struct {
				OpenClose bool `json:"openClose"`
				Change    int  `json:"change"`
			}
			if json.Unmarshal(raw, &options) == nil {
				server.syncOpenClose = options.OpenClose
				server.syncChange = options.Change
			}
		}
	}
	if err == nil {
		if raw := server.capabilities["positionEncoding"]; len(raw) > 0 {
			if json.Unmarshal(raw, &server.encoding) != nil {
				err = failure("protocol", "invalid position encoding")
			}
		}
		if server.encoding != "utf-8" && server.encoding != "utf-16" && server.encoding != "utf-32" {
			err = failure("unsupported", "server position encoding is unsupported")
		}
	}
	if err == nil {
		err = rpc.notify(ctx, "initialized", map[string]any{})
	}
	if err != nil {
		rpc.cancel()
		<-rpc.done
		return nil, err
	}
	m.servers[language] = server
	return server, nil
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := make([]*instance, 0, len(m.servers))
	for _, server := range m.servers {
		servers = append(servers, server)
	}
	m.mu.Unlock()
	for _, server := range servers {
		if server.mu.TryLock() {
			if server.rpc.live() {
				ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
				_, _ = server.rpc.request(ctx, "shutdown", nil)
				_ = server.rpc.notify(ctx, "exit", nil)
				cancel()
			}
			server.mu.Unlock()
		}
		server.rpc.cancel()
	}
	m.cancel()
	for _, server := range servers {
		<-server.rpc.done
	}
	return nil
}

func (m *Manager) Execute(ctx context.Context, operationID string, request Request) Result {
	result := Result{Version: 1, Action: request.Action}
	if err := request.Validate(); err != nil {
		return resultError(result, failure("invalid", err.Error()))
	}
	ctx = permission.WithPolicy(ctx, permission.FromContext(m.ctx))
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if err := permission.FromContext(ctx).CheckTool("LSP"); err != nil {
		return resultError(result, err)
	}
	if err := permission.FromContext(ctx).CheckProcess(); err != nil {
		return resultError(result, err)
	}
	if request.Action == "rename" || request.Action == "apply_code_action" {
		if saved, found, err := m.mutation.Recover(ctx, operationID); err != nil {
			return resultError(result, err)
		} else if found {
			result.Code = string(saved.Code)
			result.Mutation = &saved
			return result
		}
	}
	if request.Action == "restart" || request.Action == "shutdown" {
		if request.Generation == "" {
			return resultError(result, failure("invalid", "generation is required for lifecycle controls"))
		}
		server, err := m.get(ctx, request.Language, request.Generation)
		if err != nil {
			return resultError(result, err)
		}
		server.mu.Lock()
		if server.rpc.live() {
			_, _ = server.rpc.request(ctx, "shutdown", nil)
			_ = server.rpc.notify(ctx, "exit", nil)
		}
		server.rpc.cancel()
		<-server.rpc.done
		server.mu.Unlock()
		if request.Action == "shutdown" {
			result.Code = "ok"
			result.Generation = server.generation
			return result
		}
		request.Generation = ""
	}
	server, err := m.get(ctx, request.Language, request.Generation)
	if err != nil {
		return resultError(result, err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if ctx.Err() != nil {
		return resultError(result, ctx.Err())
	}
	result.Generation = server.generation
	result.Encoding = server.encoding
	if !server.rpc.live() {
		return resultError(result, failure("expired", "server generation is no longer active"))
	}
	if request.Action == "restart" {
		result.Code = "ok"
		return result
	}
	if request.Action == "apply_code_action" {
		selected, ok := server.actions[request.ActionID]
		if !ok || selected.generation != server.generation {
			return resultError(result, failure("expired", "code action is no longer available"))
		}
		delete(server.actions, request.ActionID)
		return m.apply(ctx, operationID, server, selected.edit, selected.snapshots, selected.versions, result)
	}
	snapshots := make(map[string]mutation.Snapshot)
	if request.Action == "rename" || request.Action == "code_actions" {
		snapshots, err = m.workspace(ctx, server)
		if err != nil {
			return resultError(result, err)
		}
	}
	var source mutation.Snapshot
	if request.Path != "" {
		source, err = m.mutation.Snapshot(ctx, request.Path)
		if err != nil {
			return resultError(result, err)
		}
		if err = m.synchronize(ctx, server, source); err != nil {
			return resultError(result, err)
		}
		snapshots[source.Path] = source
	}
	uri := fileURI(source.Path)
	encoding := request.Encoding
	if encoding == "" {
		encoding = "utf-16"
	}
	var position Position
	if source.Path != "" {
		offset, err := positionOffset(source.Data, request.Position, encoding)
		if err != nil {
			return resultError(result, err)
		}
		position = offsetPosition(source.Data, offset, server.encoding)
	}
	params := map[string]any{"textDocument": map[string]any{"uri": uri}, "position": position}
	method := ""
	capability := ""
	switch request.Action {
	case "definition":
		method = "textDocument/definition"
		capability = "definitionProvider"
	case "references":
		method = "textDocument/references"
		capability = "referencesProvider"
		params["context"] = map[string]any{"includeDeclaration": true}
	case "hover":
		method = "textDocument/hover"
		capability = "hoverProvider"
	case "document_symbols":
		method = "textDocument/documentSymbol"
		capability = "documentSymbolProvider"
	case "workspace_symbols":
		method = "workspace/symbol"
		capability = "workspaceSymbolProvider"
		params = map[string]any{"query": request.Query}
	case "rename":
		method = "textDocument/rename"
		capability = "renameProvider"
		params["newName"] = request.NewName
	case "code_actions":
		method = "textDocument/codeAction"
		capability = "codeActionProvider"
		span := Range{Start: position, End: position}
		if request.Range != nil {
			start, err := positionOffset(source.Data, request.Range.Start, encoding)
			if err != nil {
				return resultError(result, err)
			}
			end, err := positionOffset(source.Data, request.Range.End, encoding)
			if err != nil || start > end {
				return resultError(result, failure("invalid", "invalid code-action range"))
			}
			span = Range{offsetPosition(source.Data, start, server.encoding), offsetPosition(source.Data, end, server.encoding)}
		}
		params["range"] = span
		params["context"] = map[string]any{"diagnostics": []any{}}
	case "diagnostics":
		if server.supports("diagnosticProvider") {
			method = "textDocument/diagnostic"
		} else {
			published, err := server.rpc.awaitDiagnostics(ctx, uri, server.documents[source.Path].version)
			if err != nil {
				return resultError(result, err)
			}
			if len(published.Diagnostics) == 0 {
				return resultError(result, failure("limit", "diagnostics exceed result limit"))
			}
			result.Data = published.Diagnostics
			result.Code = "ok"
			return bounded(result)
		}
	}
	if capability != "" && !server.supports(capability) {
		return resultError(result, failure("unsupported", "language server does not advertise this capability"))
	}
	data, err := server.rpc.request(ctx, method, params)
	if err != nil {
		return resultError(result, err)
	}
	if source.Path != "" {
		latest, err := m.mutation.Snapshot(ctx, source.Path)
		if err != nil {
			return resultError(result, err)
		}
		if latest.Revision != source.Revision {
			return resultError(result, failure("stale", "document changed during semantic request"))
		}
	}
	if request.Action == "rename" {
		var edit workspaceEdit
		if string(data) == "null" {
			result.Code = "ok"
			result.Data = data
			return result
		}
		if json.Unmarshal(data, &edit) != nil {
			return resultError(result, failure("protocol", "invalid rename workspace edit"))
		}
		return m.apply(ctx, operationID, server, edit, snapshots, versions(server), result)
	}
	if request.Action == "code_actions" {
		var candidates []action
		if json.Unmarshal(data, &candidates) != nil {
			return resultError(result, failure("protocol", "invalid code actions"))
		}
		if len(candidates) > 128 {
			return resultError(result, failure("limit", "too many code actions"))
		}
		summaries := make([]ActionSummary, 0, len(candidates))
		// Previous actions expire at the next query; no unbounded server-side cache.
		clear(server.actions)
		for _, candidate := range candidates {
			summary := ActionSummary{Title: candidate.Title, Kind: candidate.Kind}
			if len(summary.Title) > 4096 {
				summary.Title = summary.Title[:4096]
			}
			unsupported := candidate.Edit == nil || len(candidate.Command) > 0 && string(candidate.Command) != "null" || len(candidate.Disabled) > 0 && string(candidate.Disabled) != "null"
			if !unsupported {
				if _, err := m.prepare(*candidate.Edit, snapshots, versions(server), server.encoding); err != nil {
					unsupported = true
				}
			}
			summary.Unsupported = unsupported
			if unsupported {
				summary.Reason = "requires command execution, resolution, or an unsupported workspace edit"
			}
			if !unsupported {
				summary.ID = uuid.New().String()
				server.actions[summary.ID] = preparedAction{server.generation, *candidate.Edit, snapshots, versions(server)}
			}
			summaries = append(summaries, summary)
		}
		result.Data, _ = json.Marshal(summaries)
		result.Code = "ok"
		return bounded(result)
	}
	result.Data = data
	result.Code = "ok"
	return bounded(result)
}
func resultError(result Result, err error) Result {
	result.Code = errorCode(err)
	result.Denial = permission.Failure(err)
	var typed *Error
	switch {
	case result.Denial != nil:
		result.Message = result.Denial.Error()
	case errors.As(err, &typed):
		result.Message = typed.Message
	default:
		result.Message = "language-server operation failed"
	}
	return result
}
func bounded(result Result) Result {
	if len(result.Data) > MaxResultBytes {
		result.Data = nil
		return resultError(result, failure("limit", "semantic result exceeds byte limit"))
	}
	return result
}
func (s *instance) supports(name string) bool {
	raw := s.capabilities[name]
	return len(raw) > 0 && string(raw) != "false" && string(raw) != "null"
}
func versions(s *instance) map[string]int {
	values := make(map[string]int, len(s.documents))
	for path, doc := range s.documents {
		values[path] = doc.version
	}
	return values
}
func (m *Manager) synchronize(ctx context.Context, s *instance, snapshot mutation.Snapshot) error {
	if !snapshot.Revision.Exists {
		return failure("unavailable", "document does not exist")
	}
	if !utf8.Valid(snapshot.Data) || strings.ContainsRune(string(snapshot.Data), 0) {
		return failure("unsupported", "document is not UTF-8 text")
	}
	if len(snapshot.Data) > maxMessageBytes/2 {
		return failure("limit", "document exceeds synchronization limit")
	}
	if !s.syncOpenClose || (s.syncChange != 1 && s.syncChange != 2) {
		return failure("unsupported", "server does not support document synchronization")
	}
	previous, exists := s.documents[snapshot.Path]
	total := len(snapshot.Data) - len(previous.snapshot.Data)
	for _, doc := range s.documents {
		total += len(doc.snapshot.Data)
	}
	if total > MaxWorkspaceBytes {
		return failure("limit", "synchronized text exceeds byte limit")
	}
	if previous.version >= 2147483647 {
		return failure("expired", "document version exhausted; restart server")
	}

	if exists && previous.snapshot.Revision == snapshot.Revision {
		return nil
	}
	version := previous.version + 1
	if exists {
		if err := s.rpc.notify(ctx, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": fileURI(snapshot.Path), "version": version}, "contentChanges": []any{map[string]any{"text": string(snapshot.Data)}}}); err != nil {
			return err
		}
	} else {
		if len(s.documents) >= MaxDocuments {
			return failure("limit", "too many synchronized documents")
		}
		if err := s.rpc.notify(ctx, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(snapshot.Path), "languageId": s.config.Language, "version": version, "text": string(snapshot.Data)}}); err != nil {
			return err
		}
	}
	s.documents[snapshot.Path] = document{snapshot, version}
	return nil
}
func (m *Manager) workspace(ctx context.Context, s *instance) (map[string]mutation.Snapshot, error) {
	snapshots := make(map[string]mutation.Snapshot)
	total := 0
	err := filepath.WalkDir(m.mutation.Root(), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return failure("unavailable", "workspace cannot be observed")
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if _, err := m.mutation.Path(path); err != nil {
			return nil
		}
		snapshot, err := m.mutation.Snapshot(ctx, path)
		if err != nil {
			return err
		}
		total += len(snapshot.Data)
		if len(snapshots) >= MaxDocuments || total > MaxWorkspaceBytes {
			return failure("limit", "workspace exceeds semantic edit snapshot limit")
		}
		snapshots[snapshot.Path] = snapshot
		matches := len(s.config.Extensions) == 0
		for _, ext := range s.config.Extensions {
			if filepath.Ext(path) == ext {
				matches = true
			}
		}
		if matches && utf8.Valid(snapshot.Data) && !strings.ContainsRune(string(snapshot.Data), 0) {
			if err = m.synchronize(ctx, s, snapshot); err != nil {
				return err
			}
		}
		return nil
	})
	return snapshots, err
}
