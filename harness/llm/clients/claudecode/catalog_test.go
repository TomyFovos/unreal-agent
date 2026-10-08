//go:build linux || darwin

package claudecode

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

const discoveredModels = `[
 {"value":"alias-a","resolvedModel":"wire-a","displayName":"Discovered A","description":"Synthetic metadata","supportsEffort":true,"supportedEffortLevels":["low","medium","future"],"supportsAdaptiveThinking":true,"supportsFastMode":false},
 {"value":"alias-b","resolvedModel":"wire-b","displayName":"Discovered B","description":"Synthetic metadata","supportsEffort":true,"supportedEffortLevels":["high","xhigh","max"]},
 {"value":"alias-no-effort","resolvedModel":"wire-no-effort","displayName":"No effort model","supportsEffort":false,"supportedEffortLevels":[]}
]`

func TestCatalogDiscoveryUsesIsolatedControlWithoutInference(t *testing.T) {
	c, f := fakeClient(t)
	c.config.ManagedPolicyMode = ManagedPolicyTrust
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: discoveredModels})
	c.config.Getenv = func(key string) string {
		if key == "HOME" {
			return f.Home
		}
		if key == "ANTHROPIC_API_KEY" || key == "CLAUDE_CODE_OAUTH_TOKEN" || key == "OTEL_EXPORTER_OTLP_HEADERS" || key == "CLAUDE_CODE_SIMPLE" {
			return "parent-sensitive"
		}
		return ""
	}
	catalog, err := c.Catalog(t.Context())
	if err != nil || !catalog.Available || !catalog.Authoritative || len(catalog.Models) != 3 {
		t.Fatal("control discovery failed", err)
	}
	m := catalog.Models[0]
	if m.ID != "alias-a" || m.ResolvedModel != "wire-a" || m.Name != "Discovered A" || !slices.Equal(m.Efforts, []llm.ReasoningEffort{"low", "medium"}) || !m.UnsupportedEfforts || m.AdaptiveThinking == nil || !*m.AdaptiveThinking {
		t.Fatal("model/effort metadata was lost")
	}
	if !catalog.Models[2].AllowsEffort("") || catalog.Models[2].AllowsEffort("medium") {
		t.Fatal("invented effort for a model without effort support")
	}
	if m2, e := catalog.FindSelection("wire-a"); e != nil || m2.ID != "alias-a" {
		t.Fatal("alias and wire identity conflated", e)
	}
	catalog.Models[0].Efforts[0] = "high"
	*catalog.Models[0].SupportsEffort = false
	again, err := c.Catalog(t.Context())
	if err != nil || again.Models[0].Efforts[0] != "low" || !*again.Models[0].SupportsEffort || len(f.CatalogCalls(t)) != 1 {
		t.Fatal("cache mutated or opened another process", err)
	}
	if len(f.Calls(t)) != 0 {
		t.Fatal("discovery sent inference")
	}
	call := f.CatalogCalls(t)[0]
	if err := validateLaunchContract(call.Arguments); err != nil {
		t.Fatal(err)
	}
	argument(t, call.Arguments, "--input-format", "stream-json")
	argument(t, call.Arguments, "--output-format", "stream-json")
	argument(t, call.Arguments, "--system-prompt-file", os.DevNull)
	argument(t, call.Arguments, "--permission-prompts", "none")
	for _, flag := range []string{"-p", "--print", "--bare", "--continue", "--resume", "--model", "--effort"} {
		if slices.Contains(call.Arguments, flag) {
			t.Fatal("discovery used a turn/configuration command", flag)
		}
	}
	if !slices.Contains(call.Arguments, "--no-session-persistence") {
		t.Fatal("discovery persisted a Claude session")
	}
	for _, env := range call.Environment {
		if strings.Contains(env, "parent-sensitive") || strings.HasPrefix(env, "ANTHROPIC_API_KEY=") || strings.HasPrefix(env, "OTEL_") || strings.HasPrefix(env, "CLAUDE_CODE_SIMPLE=") {
			t.Fatal("parent credential/routing env inherited")
		}
	}
	lines := strings.Split(strings.TrimSpace(call.Input), "\n")
	if len(lines) != 2 {
		t.Fatal("unexpected control commands")
	}
	for i, line := range lines {
		var request struct {
			Type    string `json:"type"`
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal([]byte(line), &request) != nil || request.Type != "control_request" || request.Request.Subtype != []string{"initialize", "list_models"}[i] {
			t.Fatal("discovery sent user/tool content")
		}
	}
	data, _ := json.Marshal(again)
	if strings.Contains(string(data), "sensitive") {
		t.Fatal("initialize account data entered catalog")
	}
}

func TestCatalogRestrictionsRefreshAndUpgrade(t *testing.T) {
	c, f := fakeClient(t)
	f.Set(t, testclaude.Config{Catalog: discoveredModels})
	if _, err := c.Catalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.Set(t, testclaude.Config{Catalog: `[{"value":"restricted-only","displayName":"Allowed","supportsEffort":true,"supportedEffortLevels":["high"]}]`})
	c.InvalidateCatalog()
	cat, err := c.Catalog(t.Context())
	if err != nil || len(cat.Models) != 1 || cat.Models[0].ID != "restricted-only" || len(f.CatalogCalls(t)) != 2 {
		t.Fatal("organization restrictions did not replace old/explicit models", err)
	}
	info, err := os.Stat(f.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(f.Binary, time.Now(), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Catalog(t.Context()); err != nil || len(f.CatalogCalls(t)) != 3 {
		t.Fatal("upgrade failed to invalidate catalog", err)
	}
	f.Set(t, testclaude.Config{Catalog: `[]`})
	c.InvalidateCatalog()
	cat, err = c.Catalog(t.Context())
	if err != nil || !cat.Available || !cat.Authoritative || len(cat.Models) != 0 {
		t.Fatal("empty organization catalog fell back to invented/explicit models", err)
	}
}

func TestCatalogUnavailableAndInitializeEquivalent(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(map[bool]string{true: "explicit fallback", false: "current only"}[explicit], func(t *testing.T) {
			c, f := fakeClient(t)
			if !explicit {
				c.catalog, _ = modelcatalog.Configured(nil)
			}
			cat, err := c.Catalog(t.Context())
			if err != nil || cat.Authoritative || cat.Available != explicit || !strings.Contains(cat.Problem, "unavailable") || len(f.Calls(t)) != 0 {
				t.Fatal("unsafe catalog fallback", err)
			}
			if explicit && len(cat.Models) != 2 || !explicit && len(cat.Models) != 0 {
				t.Fatal("fallback list changed")
			}
		})
	}
	c, f := fakeClient(t)
	f.Set(t, testclaude.Config{Catalog: discoveredModels, InitializeModelsOnly: true})
	cat, err := c.Catalog(t.Context())
	if err != nil || !cat.Authoritative || len(cat.Models) != 3 {
		t.Fatal("official supportedModels initialize equivalent unavailable", err)
	}
}

func TestCatalogRejectsTurnOrToolOutput(t *testing.T) {
	for _, stream := range []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"private-sensitive"}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"private-sensitive"}`,
		`{"type":"control_request","request_id":"private-sensitive","request":{"subtype":"can_use_tool","tool_name":"Bash"}}`,
		`{"type":"system","subtype":"init","tools":["Bash"],"mcp_servers":[],"permissionMode":"default"}`,
	} {
		t.Run(stream, func(t *testing.T) {
			c, f := fakeClient(t)
			f.Set(t, testclaude.Config{CatalogStream: stream + "\n"})
			_, err := c.Catalog(t.Context())
			requireCode(t, err, "tools_unsupported")
			if strings.Contains(err.Error(), "sensitive") || len(f.Calls(t)) != 0 {
				t.Fatal("unsafe discovery output or inference")
			}
		})
	}
	var writes bytes.Buffer
	_, err := readCatalogControls(strings.NewReader(`{"type":"control_response","response":{"subtype":"success","request_id":"unreal_catalog_init","response":{"models":"private-sensitive"}}}`+"\n"), &writes)
	requireCode(t, err, "catalog_unavailable")
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatal("raw control response leaked")
	}
	requireCode(t, writeCatalogControl(&writes, "id", "mcp_call"), "catalog_unavailable")
}

func TestCatalogCancellationReapsProcesses(t *testing.T) {
	c, f := fakeClient(t)
	f.Set(t, testclaude.Config{CatalogWait: true, CatalogChild: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Catalog(ctx); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.CatalogCalls(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	calls := f.CatalogCalls(t)
	if len(calls) != 1 {
		t.Fatal("control process did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancel was normalized to catalog failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control pipe/process leaked")
	}
	for _, pid := range []int{calls[0].PID, calls[0].ChildPID} {
		if pid != 0 {
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatal("unreaped discovery process", pid, err)
			}
		}
	}
}

func TestCatalogOptionalInitRetainsStreamIsolation(t *testing.T) {
	const init = `{"type":"system","subtype":"init","model":"wire-a","tools":[],"mcp_servers":[],"permissionMode":"default","plugins":[{"name":"plugin-sensitive"}],"agents":["agent-sensitive"],"skills":["skill-sensitive"]}` + "\n"
	var models []modelInfo
	if err := json.Unmarshal([]byte(discoveredModels), &models); err != nil {
		t.Fatal(err)
	}
	modelJSON, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	controls := `{"type":"control_response","response":{"subtype":"success","request_id":"unreal_catalog_init","response":{"models":` + string(modelJSON) + `}}}` + "\n" +
		`{"type":"control_response","response":{"subtype":"success","request_id":"unreal_catalog_models","response":{"models":` + string(modelJSON) + `}}}` + "\n"
	var writes bytes.Buffer
	catalog, err := readCatalogControls(strings.NewReader(init+controls), &writes)
	if err != nil || len(catalog.Models) != 3 {
		t.Fatal("safe optional init prevented metadata discovery", err)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil || strings.Contains(string(encoded), "sensitive") {
		t.Fatal("init catalogs entered model metadata", err)
	}
	for _, tc := range []struct{ stream, code, reason string }{
		{init + init, "tools_unsupported", "init_duplicate"},
		{`{"type":"system","subtype":"init","tools":[],"mcp_servers":[],"permissionMode":"default"}`, "malformed_stream", "init_missing_required_field"},
		{`{"type":"system","subtype":"init","model":"wire-a","tools":null,"mcp_servers":[],"permissionMode":"default"}`, "malformed_stream", "init_null_required_field"},
		{`{"type":"system","subtype":"init","model":"wire-a","tools":[],"mcp_servers":[],"permissionMode":"bypassPermissions"}`, "tools_unsupported", "init_permission_mode"},
	} {
		writes.Reset()
		_, err := readCatalogControls(strings.NewReader(tc.stream+"\n"), &writes)
		requireCode(t, err, tc.code)
		var failure *Error
		if !errors.As(err, &failure) || failure.StreamReason() != tc.reason {
			t.Fatal("discovery lost closed init diagnostics", err)
		}
	}
}

func TestCatalogRefreshAndInferenceHaveIndependentProcessesAndPipes(t *testing.T) {
	c, f := fakeClient(t)
	c.config.ManagedPolicyMode = ManagedPolicyTrust
	gate := filepath.Join(f.Directory, "inference-ready")
	f.Set(t, testclaude.Config{Subscription: "team", Gate: gate, Catalog: `[{"value":"claude-configured-a","resolvedModel":"wire-a","displayName":"Allowed","supportsEffort":true,"supportedEffortLevels":["medium"]}]`})
	if _, err := c.Catalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	catalogCall := f.CatalogCalls(t)[0]
	if err := syscall.Kill(catalogCall.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("catalog process remained live", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		response llm.Response
		err      error
	}
	done := make(chan outcome, 1)
	go func() { r, err := c.Respond(ctx, textRequest(), llm.RequestOptions{}); done <- outcome{r, err} }()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.Calls(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	calls := f.Calls(t)
	if len(calls) != 1 {
		t.Fatal("inference process did not start")
	}
	c.InvalidateCatalog()
	if _, err := c.Catalog(t.Context()); err != nil {
		t.Fatal("refresh interrupted text stream", err)
	}
	catalogCalls := f.CatalogCalls(t)
	if len(catalogCalls) != 2 {
		t.Fatal("explicit refresh reused the inference process")
	}
	for _, call := range catalogCalls {
		if call.PID == calls[0].PID || call.Directory == calls[0].Directory || slices.Contains(call.Arguments, "-p") {
			t.Fatal("discovery/inference execution reused")
		}
		if err := syscall.Kill(call.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatal("catalog process was not reaped", err)
		}
		if _, err := os.Stat(call.Directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("catalog cwd was not removed", err)
		}
	}
	if err := os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || len(result.response.Output) != 1 || result.response.Output[0].Data.(llm.Message).Text != "fake reply" {
			t.Fatal("control metadata contaminated inference", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("separate text pipe did not finish")
	}
	if err := syscall.Kill(calls[0].PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("inference process was not reaped", err)
	}
}
