//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// The background Host of the launcher tests is this test binary; it reads the
// recorded public list instead of opencode.ai.
func init() {
	if path := os.Getenv("UA_TEST_OPENCODE_GO_MODELS"); path != "" {
		readOpenCodeGoModels = func(context.Context) modelcatalog.Catalog {
			data, err := os.ReadFile(path)
			if err != nil {
				return modelcatalog.Catalog{Problem: "catalog unavailable"}
			}
			return modelcatalog.ParseOpenCodeGo(data, opencodego.OfferedModels())
		}
	}
}

func openCodeGoTestList(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "harness", "modelcatalog", "testdata", "opencode-go-models-2026-11-05.json"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func storeKey(t *testing.T, directory string, ref credential.Reference, secret string) {
	t.Helper()
	store, err := credential.OpenLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authflow.New(credential.NewManager(store)).Login(t.Context(), authflow.LoginRequest{Reference: ref, Secret: credential.NewSecret(secret)}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeGoBootstrapRequiresExplicitSelection(t *testing.T) {
	data, err := os.ReadFile(openCodeGoTestList(t))
	if err != nil {
		t.Fatal(err)
	}
	original := readOpenCodeGoModels
	readOpenCodeGoModels = func(context.Context) modelcatalog.Catalog {
		return modelcatalog.ParseOpenCodeGo(data, opencodego.OfferedModels())
	}
	t.Cleanup(func() { readOpenCodeGoModels = original })
	home := privateCLIDirectory(t)
	store := filepath.Join(home, "credentials")
	storeKey(t, store, openCodeGoKey, "fake-explicit-go-key")
	o := launchOptions{credentials: store, bootstrapCodexDisabled: true, bootstrapClaudeDisabled: true}
	getenv := func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}
	choose := func([]string) (string, error) {
		t.Fatal("a saved API key entered automatic provider selection")
		return "", nil
	}
	config, err := bootstrapRuntime(t.Context(), o, choose, getenv)
	if err != nil || config.Runtime.Provider.Provider != loginProvider || config.Providers[opencodego.ProviderID].Provider.Auth != openCodeGoKey {
		t.Fatal("stored Go key selected inference without an explicit choice", err)
	}
	explicit := func(key string) string {
		if key == "UNREAL_HARNESS_LLM_PROVIDER" {
			return opencodego.ProviderID
		}
		return getenv(key)
	}
	config, err = bootstrapRuntime(t.Context(), o, choose, explicit)
	if err != nil || config.Runtime.Provider.Provider != opencodego.ProviderID || config.Runtime.Provider.Auth != openCodeGoKey {
		t.Fatal("explicit Go selection failed", err)
	}
	o.credentials = filepath.Join(home, "missing-store")
	if _, err = bootstrapRuntime(t.Context(), o, choose, explicit); err == nil {
		t.Fatal("unavailable explicit Go selection silently used another route")
	}
}

func TestOpenCodeGoDiscoveryNeedsStoredKey(t *testing.T) {
	data, err := os.ReadFile(openCodeGoTestList(t))
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	listed := true
	original := readOpenCodeGoModels
	readOpenCodeGoModels = func(context.Context) modelcatalog.Catalog {
		reads++
		if !listed {
			return modelcatalog.Catalog{Problem: "catalog unavailable", ProblemCode: "opencode_go_list_unreachable"}
		}
		return modelcatalog.ParseOpenCodeGo(data, opencodego.OfferedModels())
	}
	t.Cleanup(func() { readOpenCodeGoModels = original })
	discover := func(store string) serveConfiguration {
		t.Helper()
		config := serveConfiguration{Launcher: &normalLauncherConfig{Version: 1, CodexDisabled: true, ClaudeDisabled: true}}
		config, err := registerNormalProviders(t.Context(), config, "", store, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	missing := filepath.Join(privateCLIDirectory(t), "credentials")
	other := filepath.Join(privateCLIDirectory(t), "credentials")
	storeKey(t, other, credential.Reference{Provider: "openai", Method: credential.APIKey, ID: "work"}, "sk-other-sensitive")
	for name, store := range map[string]string{"no_store": "", "store_missing": missing, "other_key_only": other} {
		config := discover(store)
		if usesOpenCodeGo(config) || len(config.providerStatuses) != 2 || len(config.Providers) != 0 {
			t.Fatal(name, "OpenCode Go appeared without its key", config.providerStatuses)
		}
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("discovery created the credential store", err)
	}
	if reads != 0 {
		t.Fatal("the model list was read without a stored key", reads)
	}

	stored := filepath.Join(privateCLIDirectory(t), "credentials")
	storeKey(t, stored, openCodeGoKey, "opencode-go-key-sensitive")
	config := discover(stored)
	p, ok := config.Providers[opencodego.ProviderID]
	if !ok || reads != 1 {
		t.Fatal("stored key did not register OpenCode Go", config.providerDiagnostics)
	}
	s := p.Provider
	if s.Model.ID != "deepseek-v4.1-flash" || s.Endpoint != opencodego.BaseURL || s.Auth != openCodeGoKey || s.MaxAttempts != opencodego.DefaultMaxAttempts || s.MaxStreamAttempts != opencodego.DefaultMaxStreamAttempts || p.ReasoningEffort != llm.ReasoningEffortHigh {
		t.Fatal("registration", s, p.ReasoningEffort)
	}
	if !slices.ContainsFunc(config.providerStatuses, func(m modelcatalog.Provider) bool {
		return m.ID == opencodego.ProviderID && m.Name == "OpenCode Go" && m.Availability == "available"
	}) {
		t.Fatal("status", config.providerStatuses)
	}

	// A list that cannot be read leaves OpenCode Go unavailable, never another route.
	listed = false
	config = discover(stored)
	if _, ok := config.Providers[opencodego.ProviderID]; ok || len(config.Providers) != 0 || !usesOpenCodeGo(config) {
		t.Fatal("unreadable list registered a provider")
	}
	if !slices.ContainsFunc(config.providerStatuses, func(m modelcatalog.Provider) bool {
		return m.ID == opencodego.ProviderID && m.Availability == "unavailable" && m.Reason == modelcatalog.CatalogUnavailable
	}) || len(config.providerDiagnostics) != 1 || !strings.Contains(config.providerDiagnostics[0], "OpenCode Go のモデル一覧") {
		t.Fatal("unavailable status", config.providerStatuses, config.providerDiagnostics)
	}
}

func TestOpenCodeGoOriginAndStoreLookup(t *testing.T) {
	config := serveConfiguration{}
	config.Permissions.NetworkOrigins = []string{"https://chatgpt.com"}
	if usesOpenCodeGo(config) {
		t.Fatal("unused OpenCode Go")
	}
	config.providerStatuses = []modelcatalog.Provider{unavailableProvider(opencodego.ProviderID, modelcatalog.CatalogUnavailable)}
	if !usesOpenCodeGo(config) {
		t.Fatal("a stored key whose list failed is still OpenCode Go")
	}
	allowed := withOpenCodeGoOrigin(withOpenCodeGoOrigin(config))
	if !slices.Equal(allowed.Permissions.NetworkOrigins, []string{"https://chatgpt.com", "https://opencode.ai"}) || len(config.Permissions.NetworkOrigins) != 1 {
		t.Fatal("origins", allowed.Permissions.NetworkOrigins, config.Permissions.NetworkOrigins)
	}

	home := privateCLIDirectory(t)
	fallback := filepath.Join(home, "credentials")
	if openCodeGoStore("", fallback) != "" || openCodeGoStore("", "") != "" {
		t.Fatal("a missing default store was used")
	}
	if err := os.Symlink(home, fallback); err != nil {
		t.Fatal(err)
	}
	if openCodeGoStore("", fallback) != "" {
		t.Fatal("a symlinked default store was used")
	}
	if err := os.Remove(fallback); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fallback, 0700); err != nil {
		t.Fatal(err)
	}
	if openCodeGoStore("", fallback) != fallback || openCodeGoStore("/operator/store", fallback) != "/operator/store" {
		t.Fatal("store lookup")
	}
}

// The default store resolves only OpenCode Go's key, and is never created.
func TestDefaultOpenCodeGoStoreResolvesOnlyItsKey(t *testing.T) {
	fallback := filepath.Join(privateCLIDirectory(t), "credentials")
	other := credential.Reference{Provider: "openai", Method: credential.APIKey, ID: "work"}
	resolver := withDefaultOpenCodeGoStore(nil, fallback)
	if _, err := resolver.Resolve(t.Context(), openCodeGoKey); !credential.IsCode(err, "store_required") {
		t.Fatal("missing store", err)
	}
	if _, err := os.Lstat(fallback); !os.IsNotExist(err) {
		t.Fatal("resolving created the store", err)
	}
	storeKey(t, fallback, openCodeGoKey, "opencode-go-key-sensitive")
	storeKey(t, fallback, other, "sk-other-sensitive")
	m, err := resolver.Resolve(t.Context(), openCodeGoKey)
	if err != nil || m.Token.Reveal() != "opencode-go-key-sensitive" {
		t.Fatal("stored key", err)
	}
	if _, err := resolver.Resolve(t.Context(), other); !credential.IsCode(err, "store_required") {
		t.Fatal("another provider's key came from the default store", err)
	}
}

func TestLauncherPassesDefaultCredentialStoreOutsideBinding(t *testing.T) {
	home := privateCLIDirectory(t)
	getenv := func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}
	parse := func(args ...string) launchOptions {
		t.Helper()
		o, err := parseLaunchOptions(args, io.Discard, getenv)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	store := filepath.Join(home, ".local", "state", "unreal-agent", "credentials")
	o := parse()
	args := strings.Join(backgroundArguments(o), " ")
	if o.credentials != "" || o.defaultCredentials != store || !strings.Contains(args, "--default-credential-directory "+store) || strings.Contains(args, "--credential-directory") {
		t.Fatal("default store", o.credentials, args)
	}
	if _, err := os.Lstat(store); !os.IsNotExist(err) {
		t.Fatal("the launcher created the store", err)
	}
	o = parse("--credential-directory", "/operator/store")
	args = strings.Join(backgroundArguments(o), " ")
	if o.credentials != "/operator/store" || !strings.Contains(args, "--credential-directory /operator/store") || strings.Contains(args, "--default-credential-directory") {
		t.Fatal("explicit store", args)
	}
}

// /model-sub's choice is STATE/subagent-model.json for every Host the
// launcher starts; like the default store it is outside the Host binding.
func TestLauncherPassesSubagentModelFileOutsideBinding(t *testing.T) {
	home := privateCLIDirectory(t)
	o, err := parseLaunchOptions(nil, io.Discard, func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".local", "state", "unreal-agent", "subagent-model.json")
	if args := strings.Join(backgroundArguments(o), " "); o.subagentModel != file || !strings.Contains(args, "--subagent-model-file "+file) {
		t.Fatal("subagent model file", o.subagentModel, args)
	}
	if _, err = os.Lstat(file); !os.IsNotExist(err) {
		t.Fatal("the launcher wrote the file", err)
	}
	binding, err := json.Marshal(launcherHostBinding{})
	if err != nil || strings.Contains(string(binding), "ubagent") {
		t.Fatal("the choice is part of the Host binding", string(binding), err)
	}
}

func TestOpenCodeGoKeyCommandNeverTakesTheKeyAsText(t *testing.T) {
	command := openCodeGoKeyCommand(func(string) string { return "" })
	if !strings.HasPrefix(command, "pbpaste | ") || !strings.HasSuffix(command, " login --store ~/.local/state/unreal-agent/credentials --provider opencode-go --id primary --key-stdin") || !strings.Contains(command, "unreal-agent-auth") {
		t.Fatal(command)
	}
	command = openCodeGoKeyCommand(func(key string) string {
		if key == "XDG_STATE_HOME" {
			return "/state dir/it's"
		}
		return ""
	})
	if !strings.Contains(command, `--store '/state dir/it'\''s/unreal-agent/credentials'`) {
		t.Fatal("unquoted store", command)
	}
	if shellWord("") != "''" || shellWord("/a/b-c_d.e") != "/a/b-c_d.e" || shellWord("$(x)") != "'$(x)'" {
		t.Fatal("shell words")
	}
}

// A stored key makes OpenCode Go a provider of the running Host, next to Codex
// and Claude, without changing the initial provider or sending inference.
func TestNormalHostOffersOpenCodeGoWithStoredKey(t *testing.T) {
	t.Setenv("UA_TEST_OPENCODE_GO_MODELS", openCodeGoTestList(t))
	f, cli := normalFixture(t, true, true)
	storeKey(t, filepath.Join(os.Getenv("HOME"), ".local", "state", "unreal-agent", "credentials"), openCodeGoKey, "opencode-go-key-sensitive")
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		providers, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range providers.Providers {
			if p.ID == opencodego.ProviderID {
				found = p.Name == "OpenCode Go" && p.Availability == "available" && p.Tools
			}
		}
		if !found || len(providers.Providers) != 3 {
			t.Fatal("OpenCode Go is not a provider of the Host", providers.Providers)
		}
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.catalog", ID: id, Provider: opencodego.ProviderID, Refresh: true})
		if err != nil || out.Catalog == nil || !out.Catalog.Available || len(out.Catalog.Models) != len(opencodego.OfferedModels()) {
			t.Fatal("catalog", err)
		}
		model, err := out.Catalog.FindSelection("deepseek-v4.1-flash")
		if err != nil || !slices.Equal(model.Efforts, []llm.ReasoningEffort{"low", "high", "max"}) {
			t.Fatal("model", model, err)
		}
		v, err := c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		if active := multiReport(t, v).Selection; active == nil || active.Provider != claudeagent.ProviderID {
			t.Fatal("OpenCode Go changed the initial provider")
		}
		choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "opencode-go", Provider: opencodego.ProviderID, Model: model.ID, Effort: model.DefaultEffort}
		if _, err = modelExchange(ctx, c.Extension, modelRequest{Action: "model.select", ID: id, Generation: v.Generation, Selection: choice}); err != nil {
			t.Fatal("cannot switch to OpenCode Go", err)
		}
		if f.calls.Load() != 0 || len(cli.Calls(t)) != 0 {
			t.Fatal("discovery or switching sent inference")
		}
		return nil
	}}
	if err := l.run(t.Context(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	config, err := readServeConfiguration(f.o.config)
	if err != nil || config.Providers[opencodego.ProviderID].Provider.Auth != openCodeGoKey {
		t.Fatal("bootstrap did not record the OpenCode Go route", err)
	}
	// The endpoint is allowed at Host start, not written to the file.
	if slices.Contains(config.Permissions.NetworkOrigins, "https://opencode.ai") {
		t.Fatal("bootstrap wrote the OpenCode Go origin")
	}
	data, _ := os.ReadFile(f.o.config)
	assertNoCredentialLeak(t, string(data), "opencode-go-key-sensitive")
}

// A key saved while the Host runs is found by /model refresh: the running
// Host is kept, and the launcher still attaches to it afterwards.
func TestNormalHostFindsOpenCodeGoKeySavedLater(t *testing.T) {
	t.Setenv("UA_TEST_OPENCODE_GO_MODELS", openCodeGoTestList(t))
	f, cli := normalFixture(t, true, true)
	store := filepath.Join(os.Getenv("HOME"), ".local", "state", "unreal-agent", "credentials")
	offered := func(ctx context.Context, c *gateway.Client, id session.ID, refresh bool) (int, bool) {
		t.Helper()
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.providers", ID: id, Refresh: refresh})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range out.Providers {
			if p.ID == opencodego.ProviderID {
				found = p.Name == "OpenCode Go" && p.Availability == "available" && p.Tools
			}
		}
		return len(out.Providers), found
	}
	attached := 0
	l := launcher{start: f.start, attach: func(ctx context.Context, c *gateway.Client, id session.ID) error {
		attached++
		if n, found := offered(ctx, c, id, true); n != 2 || found {
			t.Fatal("OpenCode Go appeared without its key", n)
		}
		if _, err := os.Lstat(store); !os.IsNotExist(err) {
			t.Fatal("the Host created the credential store", err)
		}
		storeKey(t, store, openCodeGoKey, "opencode-go-key-sensitive")
		if n, found := offered(ctx, c, id, true); n != 3 || !found {
			t.Fatal("refresh did not find the saved key", n)
		}
		out, err := modelExchange(ctx, c.Extension, modelRequest{Action: "model.catalog", ID: id, Provider: opencodego.ProviderID})
		if err != nil || out.Catalog == nil || !out.Catalog.Available || len(out.Catalog.Models) != len(opencodego.OfferedModels()) {
			t.Fatal("catalog", err)
		}
		if _, err = out.Catalog.FindSelection("deepseek-v4.1-flash"); err != nil {
			t.Fatal("catalog", err)
		}
		v, err := c.Inspect(ctx, id, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "opencode-go-later", Provider: opencodego.ProviderID, Model: "deepseek-v4.1-flash", Effort: llm.ReasoningEffortHigh}
		if _, err = modelExchange(ctx, c.Extension, modelRequest{Action: "model.select", ID: id, Generation: v.Generation, Selection: choice}); err != nil {
			t.Fatal("cannot switch to OpenCode Go", err)
		}
		return nil
	}}
	if err := l.run(t.Context(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	l.attach = func(ctx context.Context, c *gateway.Client, id session.ID) error {
		attached++
		if n, found := offered(ctx, c, id, false); n != 3 || !found {
			t.Fatal("the running Host lost OpenCode Go", n)
		}
		return nil
	}
	if err := l.run(t.Context(), nil, io.Discard); err != nil {
		t.Fatal("the launcher did not attach after the key was saved:", err)
	}
	if attached != 2 || f.starts.Load() != 1 {
		t.Fatal("saving the key needed another Host", attached, f.starts.Load())
	}
	if f.calls.Load() != 0 || len(cli.Calls(t)) != 0 {
		t.Fatal("discovery or switching sent inference")
	}
}

// Without its key an OpenCode Go Session is told once what to do; the login
// steps for Codex and Claude do not follow (an API key has no login).
func TestOpenCodeGoUnavailableGuidanceIsNotRepeated(t *testing.T) {
	cfg := codexServeConfiguration(t.TempDir(), "https://codex.invalid")
	cfg.Providers = map[string]agentrunner.ProviderRuntime{opencodego.ProviderID: {Provider: openCodeGoRoute("deepseek-v4.1-flash")}}
	registry, err := registeredProviders(cfg.Runtime, cfg.Providers)
	if err != nil {
		t.Fatal(err)
	}
	missing := credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{}, &credential.Error{Code: "not_found"}
	})
	runtime := agentrunner.RuntimeConfig{Identity: cfg.Runtime, Providers: registry, Credentials: missing, Backends: cfg.Providers}
	reply, err := availabilityAdapter{runtime: runtime, id: opencodego.ProviderID}.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	if err != nil || len(reply.Output) != 1 {
		t.Fatal(reply, err)
	}
	message, _ := reply.Output[0].Data.(llm.Message)
	if want := "OpenCode Go を利用できません。/login の手順で API キーを保存し、/model refresh を実行してください。"; message.Text != want {
		t.Fatalf("got %q, want %q", message.Text, want)
	}
	if got := providerGuidance("openai-codex", "codex login でログインしてください", "。ログイン後は /model refresh を実行してください"); got != "codex login でログインしてください。ログイン後は /model refresh を実行してください" {
		t.Fatal("Codex lost its login steps", got)
	}
}
