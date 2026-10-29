package modelcatalog

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testcodex"
)

const codexModelPage = `{"data":[{"id":"picker-id","model":"wire-model","displayName":"Synthetic Model","description":"provider-prose-not-public","hidden":false,"isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"low","description":"private-effort-detail"},{"reasoningEffort":"high"},{"reasoningEffort":"hyper"}],"account_id":"account-private"}],"nextCursor":null}`

func codexFrames(page string) []any {
	var value any
	if json.Unmarshal([]byte(page), &value) != nil {
		panic("bad synthetic fixture")
	}
	return []any{map[string]any{"id": 1, "result": map[string]any{"userAgent": "synthetic", "codexHome": "private-path"}}, map[string]any{"id": 2, "result": value}}
}

func TestCodexCatalogProtocolMetadataOnly(t *testing.T) {
	text := `{"id":1,"result":{"userAgent":"synthetic","codexHome":"private-path"}}
{"method":"account/updated","params":{"accountId":"account-private"}}
{"method":"configWarning","params":{"summary":"provider-prose-not-public","details":"private-path"}}
{"method":"remoteControl/status/changed","params":{"installationId":"account-private","status":"disabled"}}
{"id":2,"result":` + codexModelPage + "}\n"
	var requests bytes.Buffer
	c, err := readCodexCatalog(strings.NewReader(strings.Repeat(`{"method":"new/notification","params":{}}`+"\n", 70)+text), &requests)
	if err != nil || !c.Available || len(c.Models) != 1 || c.Models[0].ID != "wire-model" || c.Models[0].DefaultEffort != "high" || !c.Models[0].UnsupportedEfforts || c.Models[0].ContextWindow != 0 {
		t.Fatal("official model/list wire metadata not normalized", err)
	}
	if !reflect.DeepEqual(c.Models[0].Efforts, []llm.ReasoningEffort{"low", "high"}) {
		t.Fatal("efforts were invented or reordered")
	}
	var methods []string
	for _, line := range strings.Split(strings.TrimSpace(requests.String()), "\n") {
		var request struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &request) != nil {
			t.Fatal("invalid request")
		}
		methods = append(methods, request.Method)
	}
	if !reflect.DeepEqual(methods, []string{"initialize", "initialized", "model/list"}) {
		t.Fatal("metadata reader sent an execution request", methods)
	}
	public, _ := json.Marshal(c)
	for _, value := range []string{"private-path", "provider-prose-not-public", "private-effort-detail", "account-private", "picker-id"} {
		if strings.Contains(string(public), value) {
			t.Fatal("private metadata leaked")
		}
	}
}

func TestCodexCatalogProtocolFailuresClosed(t *testing.T) {
	prefix := "{\"id\":1,\"result\":{\"userAgent\":\"synthetic\"}}\n"
	for _, tc := range []struct{ name, text, code string }{

		{"malformed", "{malformed\n", "codex_cli_protocol_invalid"},
		{"duplicate", `{"id":2,"id":2,"result":` + codexModelPage + "}\n", "codex_cli_protocol_invalid"},
		{"wrong-id", `{"id":9,"result":` + codexModelPage + "}\n", "codex_cli_protocol_invalid"},
		{"server-request", `{"id":2,"method":"item/commandExecution/requestApproval","params":{"command":"never-execute"}}` + "\n", "codex_cli_protocol_invalid"},

		{"denial", `{"id":2,"error":{"code":-1,"message":"secret-provider-body"}}` + "\n", "codex_cli_rejected"},
		{"null", `{"id":2,"result":null}` + "\n", "codex_cli_protocol_invalid"},
		{"no-models", `{"id":2,"result":{"data":[]}}` + "\n", "codex_cli_metadata_invalid"},
		{"wrong-type", `{"id":2,"result":{"data":"wrong"}}` + "\n", "codex_cli_metadata_invalid"},
		{"oversized", strings.Repeat("x", 1<<20) + "\n", "codex_cli_protocol_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests bytes.Buffer
			c, err := readCodexCatalog(strings.NewReader(prefix+tc.text), &requests)
			if err == nil || err.Error() != tc.code || c.Available || len(c.Models) != 0 {
				t.Fatal("unsafe or untyped protocol outcome", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(requests.String(), "never-execute") || strings.Contains(requests.String(), "thread/") || strings.Contains(requests.String(), "turn/") {
				t.Fatal("payload leaked or execution sent")
			}
		})
	}
}

func TestCodexCatalogPaginationAndBounds(t *testing.T) {
	page := strings.Replace(codexModelPage, `"nextCursor":null`, `"nextCursor":"opaque-cursor"`, 1)
	last := strings.ReplaceAll(codexModelPage, "wire-model", "second-model")
	var request bytes.Buffer
	text := "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":" + page + "}\n{\"id\":3,\"result\":" + last + "}\n"
	c, err := readCodexCatalog(strings.NewReader(text), &request)
	if err != nil || len(c.Models) != 2 || !strings.Contains(request.String(), `"cursor":"opaque-cursor"`) || strings.Contains(c.Source, "opaque-cursor") {
		t.Fatal("pagination lost or exposed", err)
	}
	text = "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":" + page + "}\n{\"id\":3,\"result\":" + page + "}\n"
	if _, err = readCodexCatalog(strings.NewReader(text), &bytes.Buffer{}); err == nil || err.Error() != "codex_cli_protocol_invalid" {
		t.Fatal("cursor loop accepted", err)
	}
	var fixture struct {
		Data []codexModelInfo `json:"data"`
	}
	if err = json.Unmarshal([]byte(codexModelPage), &fixture); err != nil {
		t.Fatal(err)
	}
	rows := make([]codexModelInfo, 257)
	for i := range rows {
		rows[i] = fixture.Data[0]
		rows[i].Model = fmt.Sprintf("model-%d", i)
	}
	large, _ := json.Marshal(map[string]any{"id": 2, "result": map[string]any{"data": rows, "nextCursor": nil}})
	if result, err := readCodexCatalog(strings.NewReader("{\"id\":1,\"result\":{}}\n"+string(large)+"\n"), &bytes.Buffer{}); err != nil || len(result.Models) != 257 {
		t.Fatal("large catalog refused", err)
	}
}

func TestCodexCatalogModelMetadataValidation(t *testing.T) {
	for _, replacement := range []struct{ from, to string }{
		{`"model":"wire-model"`, `"model":"--argument"`},
		{`"displayName":"Synthetic Model"`, `"displayName":"unsafe\u001b[2J"`},
		{`"reasoningEffort":"high"`, `"reasoningEffort":{}`},
		{`"supportedReasoningEfforts":[`, `"supportedReasoningEfforts":"bad","other":[`},
	} {
		page := strings.Replace(codexModelPage, replacement.from, replacement.to, 1)
		_, err := readCodexCatalog(strings.NewReader("{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":"+page+"}\n"), &bytes.Buffer{})
		if err == nil || err.Error() != "codex_cli_metadata_invalid" {
			t.Fatal("invalid model metadata accepted", err)
		}
	}
	page := strings.Replace(codexModelPage, `"hidden":false`, `"hidden":true`, 1)
	if _, err := readCodexCatalog(strings.NewReader("{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":"+page+"}\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("hidden-only catalog offered")
	}
}

func TestCodexCLIProcessCacheRefreshEnvironmentAndCleanup(t *testing.T) {
	home := t.TempDir()
	binary := testcodex.Install(t, home, codexFrames(codexModelPage))
	codexHome := filepath.Join(home, "codex-home")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		switch key {
		case "HOME":
			return home
		case "PATH":
			return os.Getenv("PATH")
		case "SBX_CRED_OPENAI_MODE":
			return "oauth"
		default:
			return "SECRET_MUST_NOT_BE_INHERITED"
		}
	}
	cli := &CodexCLI{Home: codexHome, Binary: binary, Getenv: getenv, Validate: func(context.Context) error { return nil }, CachePath: filepath.Join(home, "catalog-cache.json")}
	for _, refresh := range []bool{false, false, true} {
		if c := cli.Catalog(t.Context(), refresh); !c.Available || len(c.Models) != 1 {
			t.Fatal("metadata-only subprocess failed", c.ProblemCode)
		}
	}
	starts, err := os.ReadFile(filepath.Join(home, "codex-starts.jsonl"))
	if err != nil || bytes.Count(starts, []byte{'\n'}) != 2 || bytes.Contains(starts, []byte("SECRET_MUST_NOT_BE_INHERITED")) {
		t.Fatal("TTL/refresh or restricted env failed", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(starts)), "\n") {
		var record struct {
			Argv []string          `json:"argv"`
			Env  map[string]string `json:"env"`
			Cwd  string            `json:"cwd"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || !reflect.DeepEqual(record.Argv, codexCatalogArguments(record.Cwd)) || record.Env["CODEX_HOME"] != codexHome {
			t.Fatal("source/argv mismatch")
		}
		if _, err = os.Stat(record.Cwd); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("temporary CLI state was not cleaned up")
		}
		for key := range record.Env {
			if key != "HOME" && key != "PATH" && key != "CODEX_HOME" && key != "NO_COLOR" && key != "SBX_CRED_OPENAI_MODE" && key != "LC_CTYPE" {
				t.Fatal("unrelated environment inherited", key)
			}
		}
	}
	cli.Validate = func(context.Context) error { return errors.New("private-denial") }
	if c := cli.Catalog(t.Context(), false); c.Available || c.ProblemCode != "codex_oauth_source_invalid" {
		t.Fatal("stale cached metadata bypassed proxy validation")
	}
}

func TestCodexCLIProcessMissingCrashCancellationTimeout(t *testing.T) {
	for _, mode := range []string{"missing", "crash", "hang", "malformed", "canceled", "canceled-before-start"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			binary := testcodex.Install(t, home, []any{mode})
			if mode == "missing" {
				binary += ".absent"
			}
			ctx := t.Context()
			validate := func(context.Context) error { return nil }
			if mode == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mode == "canceled-before-start" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				// Expire the request after Catalog's initial check, before
				// exec.Cmd.Start, without depending on scheduler timing.
				validate = func(context.Context) error { cancel(); return nil }
			}
			cli := &CodexCLI{Home: home, CachePath: filepath.Join(home, "catalog-cache.json"), Binary: binary, Getenv: func(key string) string {
				if key == "HOME" {
					return home
				}
				return os.Getenv(key)
			}, Validate: validate}
			c := cli.Catalog(ctx, false)
			want := map[string]string{"missing": "codex_cli_missing", "crash": "codex_cli_protocol_invalid", "hang": "codex_cli_canceled", "malformed": "codex_cli_protocol_invalid", "canceled": "codex_cli_canceled", "canceled-before-start": "codex_cli_canceled"}[mode]
			if c.Available || len(c.Models) != 0 || c.ProblemCode != want {
				t.Fatal("wrong safe failure", c.ProblemCode, want)
			}
		})
	}
}

func TestCodexCacheProblemCodesAndReadOnlySource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	for _, tc := range []struct{ data, code string }{
		{`{invalid`, "codex_cache_invalid"},
		{`{"models":[]}`, "codex_cache_no_selectable_models"},
		{`{"models":[{"slug":"bad","visibility":"list","supported_reasoning_levels":[{"effort":"hyper"}]}]}`, "codex_cache_no_selectable_models"},
	} {
		if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(path)
		c := ReadCodexCache(path)
		after, _ := os.Stat(path)
		data, _ := os.ReadFile(path)
		if c.Available || c.ProblemCode != tc.code || string(data) != tc.data || !before.ModTime().Equal(after.ModTime()) || CodexProblemHint(c.ProblemCode) == "" {
			t.Fatal("cache failure changed its source or hid its reason")
		}
	}
	if CodexProblemHint("SECRET_PROVIDER_TEXT") != "" {
		t.Fatal("unsafe reason surfaced")
	}
}

func TestCodexCLIUnsafeCacheNeverLaunchesProcess(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "models_cache.json")
	if err := os.Symlink(filepath.Join(home, "credential-file"), path); err != nil {
		t.Fatal(err)
	}
	cli := &CodexCLI{Home: home, Binary: filepath.Join(home, "missing-cli"), Validate: func(context.Context) error { return nil }}
	if c := cli.Catalog(t.Context(), true); c.ProblemCode != "codex_cli_missing" || c.Available {
		t.Fatal("redirected source accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate((16 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if c := cli.Catalog(t.Context(), true); c.ProblemCode != "codex_cli_missing" || c.Available {
		t.Fatal("oversized source delegated to CLI")
	}
}

// A slow CLI start is paid once: another process reuses the saved list, and
// a failed start keeps the last good one.
func TestCodexCLISavedListAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(home, "catalog-cache.json")
	getenv := func(key string) string {
		if key == "PATH" {
			return os.Getenv("PATH")
		}
		return home
	}
	valid := func(context.Context) error { return nil }
	good := testcodex.Install(t, home, codexFrames(codexModelPage))
	if c := (&CodexCLI{Home: codexHome, Binary: good, Getenv: getenv, Validate: valid, CachePath: cache}).Catalog(t.Context(), false); !c.Available {
		t.Fatal("first discovery failed", c.ProblemCode)
	}
	crashHome := t.TempDir()
	crash := testcodex.Install(t, crashHome, []any{"crash"})
	getenv = func(key string) string {
		if key == "PATH" {
			return os.Getenv("PATH")
		}
		return crashHome
	}
	if c := (&CodexCLI{Home: codexHome, Binary: crash, Getenv: getenv, Validate: valid, CachePath: cache}).Catalog(t.Context(), false); !c.Available || len(c.Models) != 1 {
		t.Fatal("another process did not reuse the saved list")
	}
	if c := (&CodexCLI{Home: codexHome, Binary: crash, Getenv: getenv, Validate: valid, CachePath: cache}).Catalog(t.Context(), true); !c.Available {
		t.Fatal("a failed refresh lost the last good list")
	}
	if c := (&CodexCLI{Home: filepath.Join(home, "other"), Binary: crash, Getenv: getenv, Validate: valid, CachePath: cache}).Catalog(t.Context(), false); c.Available {
		t.Fatal("a list saved for another Codex home was used")
	}
}
