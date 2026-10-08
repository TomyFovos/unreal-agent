//go:build linux || darwin

package main

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

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func claudeServeConfiguration(t *testing.T, f *testclaude.Fixture) serveConfiguration {
	t.Helper()
	t.Setenv("HOME", f.Home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return serveConfiguration{
		Runtime:     agentrunner.RuntimeIdentity{Version: 1, Workspace: t.TempDir(), SystemPrompt: "system-prompt-sensitive", ReasoningEffort: "medium", Profile: profile.Default(), Provider: provider.Selection{Version: 1, Provider: "claude-code", Model: provider.Model{ID: "claude-configured-a", Family: "claude", Capabilities: []string{"reasoning"}}, Auth: credential.Reference{Provider: "claude-code", Method: credential.OAuth, ID: "external-claude-code"}, MaxAttempts: 1, Source: "operator configuration"}, ClaudeCode: &claudecode.Config{Binary: f.Binary, Models: []modelcatalog.Model{{ID: "claude-configured-a", Name: "Configured A", Efforts: []llm.ReasoningEffort{"low", "medium"}, DefaultEffort: "medium"}, {ID: "claude-configured-b", Name: "Configured B", Efforts: []llm.ReasoningEffort{"high"}, DefaultEffort: "high"}}}},
		Permissions: permission.Config{ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true},
	}
}
func cliArgument(t *testing.T, call testclaude.Call, name, want string) {
	t.Helper()
	i := slices.Index(call.Arguments, name)
	if i < 0 || i+1 >= len(call.Arguments) || call.Arguments[i+1] != want {
		t.Fatalf("wrong %s selection; expected %s", name, want)
	}
}

func TestClaudeGatewayModelBoundaryAnalysisAndHostResume(t *testing.T) {
	for _, mode := range []claudecode.ManagedPolicyMode{"", claudecode.ManagedPolicyTrust} {
		t.Run(string(mode.Effective()), func(t *testing.T) { testClaudeGatewayModelBoundaryAndResume(t, mode) })
	}
}

func testClaudeGatewayModelBoundaryAndResume(t *testing.T, mode claudecode.ManagedPolicyMode) {
	t.Helper()
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	config.Runtime.ClaudeCode.ManagedPolicyMode = mode
	t.Setenv("ANTHROPIC_API_KEY", "private-api-key-sensitive")
	gate := filepath.Join(f.Directory, "release")
	fake := testclaude.Config{Gate: gate}
	if mode == claudecode.ManagedPolicyTrust {
		fake.Subscription, fake.ManagedTelemetry = "team", true
	}
	f.Set(t, fake)
	c, store, output := startInteractiveHost(t, config)
	v, e := c.Open(t.Context(), host.Create, "claude-test")
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "claude-test"})
	if e != nil || catalog.Catalog == nil || !catalog.Catalog.Available || len(catalog.Catalog.Models) != 2 || len(catalog.Catalog.Models[1].Efforts) != 1 {
		t.Fatal("configured Claude catalog not served", e)
	}
	submitInteractive(t, c, v, "first", "first-user-sensitive")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 1 })
	cliArgument(t, f.Calls(t)[0], "--model", "claude-configured-a")
	cliArgument(t, f.Calls(t)[0], "--effort", "medium")
	choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "claude-choice", Model: "claude-configured-b", Effort: "high"}
	bad := choice
	bad.RequestID = "invalid-effort"
	bad.Effort = "medium"
	if _, e = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "claude-test", Generation: v.Generation, Selection: bad}); e == nil {
		t.Fatal("accepted common hard-coded effort for another model")
	}
	if _, e = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "claude-test", Generation: v.Generation, Selection: choice}); e != nil {
		t.Fatal(e)
	}
	busy, e := c.Inspect(t.Context(), "claude-test", 0, 256)
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range busy.History.Items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Kind == sessionstore.HostRuntimeApplied {
			t.Fatal("selection applied inside active request")
		}
	}
	if e = os.WriteFile(gate, []byte("ready"), 0600); e != nil {
		t.Fatal(e)
	}
	inspectInteractive(t, c, "claude-test", func(v host.View) bool { return responseCount(v) == 1 })
	submitInteractive(t, c, v, "second", "second-user-sensitive")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 2 })
	cliArgument(t, f.Calls(t)[1], "--model", "claude-configured-b")
	cliArgument(t, f.Calls(t)[1], "--effort", "high")
	current := inspectInteractive(t, c, "claude-test", func(v host.View) bool { return responseCount(v) == 2 })
	var revisions []uint64
	intent, applied := 0, 0
	for _, item := range current.History.Items {
		switch r := item.Data.(type) {
		case session.Turn:
			revisions = append(revisions, r.RuntimeRevision)
		case sessionstore.HostRecord:
			if r.Kind == sessionstore.HostRuntimeSelection {
				intent++
			}
			if r.Kind == sessionstore.HostRuntimeApplied {
				applied++
			}
		case sessionstore.ModelResponse:
			if len(r.Response.Output) != 1 || r.Response.Output[0].Type != llm.ItemMessage {
				t.Fatal("unexpected Claude tool output")
			}
		case sessionstore.ToolCallStatus:
			t.Fatal("text-only provider created canonical tool operations")
		}
	}
	if !slices.Equal(revisions, []uint64{0, 1}) || intent != 1 || applied != 1 {
		t.Fatal("canonical runtime revisions lost", revisions, intent, applied)
	}
	if _, e = c.Stop(t.Context(), "claude-test", v.Generation, inbox.StopWhenIdle, "test"); e != nil {
		t.Fatal(e)
	}
	inspectInteractive(t, c, "claude-test", func(v host.View) bool { return !v.Running })
	c2, _, output2 := startInteractiveHost(t, config, "--session-directory", store)
	v, e = c2.Open(t.Context(), host.Resume, "claude-test")
	if e != nil {
		t.Fatal(e)
	}
	if len(f.Calls(t)) != 2 {
		t.Fatal("resume performed speculative inference")
	}
	submitInteractive(t, c2, v, "third", "third-user-sensitive")
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 3 })
	last := f.Calls(t)[2]
	cliArgument(t, last, "--model", "claude-configured-b")
	cliArgument(t, last, "--effort", "high")
	for _, text := range []string{"first-user-sensitive", "second-user-sensitive", "third-user-sensitive", "fake reply"} {
		if !strings.Contains(last.Input, text) {
			t.Fatal("resume lost canonical text context")
		}
	}
	current = inspectInteractive(t, c2, "claude-test", func(v host.View) bool { return responseCount(v) == 3 })
	a := analysis.New("claude-test")
	a.Created(current.Session.Session.CreatedAt)
	for _, item := range current.History.Items {
		a.Apply(item)
	}
	report := a.Snapshot(time.Now(), true, false, nil, nil)
	if report.ManagedPolicyMode != string(mode.Effective()) {
		t.Fatal("resume/analysis lost administrative trust boundary", report.ManagedPolicyMode)
	}
	if report.Usage.Input != 180 || report.Usage.Output != 120 || !report.Usage.Partial || !slices.Contains(report.Usage.Unknown, llm.UsageReasoning) || report.Tools.Calls != 0 || len(report.Turns) != 3 {
		t.Fatal("Claude usage/analysis did not share projection", report.Usage)
	}
	if report.Turns[0].Selection.Model != "claude-configured-a" || report.Turns[0].Selection.Effort != "medium" || report.Turns[1].Selection.Model != "claude-configured-b" || report.Turns[2].Selection.Effort != "high" {
		t.Fatal("analysis rewrote previous model selections")
	}
	if s := strings.Join(analysis.Lines(report, "Usage", true), "\n"); !strings.Contains(s, "reasoning     unknown") {
		t.Fatal("missing reasoning treated as measured zero", s)
	}
	for _, format := range []string{"json", "markdown"} {
		b, e := analysis.Encode(report, format)
		if e != nil {
			t.Fatal(e)
		}
		assertNoCredentialLeak(t, string(b), "private-api-key-sensitive", "account-sensitive", "private-reasoning-sensitive", "private-signature-sensitive", "claude-internal-session-sensitive", "system-prompt-sensitive", "first-user-sensitive", "fake reply")
		assertNoCredentialLeak(t, string(b), "managed-telemetry-sensitive", "managed-telemetry-credential-sensitive")
	}
	if _, e = c2.Stop(t.Context(), "claude-test", v.Generation, inbox.StopWhenIdle, "test"); e != nil {
		t.Fatal(e)
	}
	inspectInteractive(t, c2, "claude-test", func(v host.View) bool { return !v.Running })
	assertNoStoredCredentials(t, store, "private-api-key-sensitive", "account-sensitive", "private-reasoning-sensitive", "private-signature-sensitive", "claude-internal-session-sensitive", "stderr-sensitive", "token-sensitive")
	assertNoStoredCredentials(t, store, "managed-telemetry-sensitive", "managed-telemetry-credential-sensitive")
	assertNoCredentialLeak(t, output.String(), "private-api-key-sensitive", "account-sensitive", "token-sensitive")
	assertNoCredentialLeak(t, output2.String(), "private-api-key-sensitive", "account-sensitive", "token-sensitive")
}

func TestClaudeStartupFailuresStayTypedAndDoNotStartGateway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config testclaude.Config
		code   string
	}{{"missing auth", testclaude.Config{SignedOut: true}, "external_reauth_required"}, {"policy-owned login", testclaude.Config{Subscription: "team"}, "policy_isolation_unavailable"}, {"unsupported version", testclaude.Config{Version: "2.1.0"}, "unsupported_version"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := testclaude.New(t)
			config := claudeServeConfiguration(t, f)
			f.Set(t, tc.config)
			b, _ := json.Marshal(config)
			dir := privateCLIDirectory(t)
			path := filepath.Join(dir, "runtime.json")
			if e := os.WriteFile(path, b, 0600); e != nil {
				t.Fatal(e)
			}
			var out bytes.Buffer
			err := run(t.Context(), []string{"serve", "--config", path, "--session-directory", filepath.Join(dir, "sessions"), "--socket", filepath.Join(dir, "host.sock")}, &out)
			var typed *claudecode.Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatal("lost safe typed startup failure", err)
			}
			if len(f.Calls(t)) != 0 {
				t.Fatal("startup sent a model request")
			}
			if tc.code == "external_reauth_required" && !credential.IsCode(err, tc.code) {
				t.Fatal("external reauth identity missing")
			}
			assertNoCredentialLeak(t, out.String(), "account-sensitive", "email-sensitive")
			if e := preflightClaudeLauncher(t.Context(), config); !errors.As(e, &typed) || typed.Code != tc.code {
				t.Fatal("launcher hid Claude startup error", e)
			}
		})
	}
}

func TestClaudeManagedPolicyConfigAndLauncherPreflight(t *testing.T) {
	for _, mode := range []claudecode.ManagedPolicyMode{"", claudecode.ManagedPolicyReject, claudecode.ManagedPolicyTrust, "private-mode-sensitive"} {
		t.Run(string(mode), func(t *testing.T) {
			f := testclaude.New(t)
			config := claudeServeConfiguration(t, f)
			config.Runtime.ClaudeCode.ManagedPolicyMode = mode
			f.Set(t, testclaude.Config{Subscription: "enterprise"})
			b, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(privateCLIDirectory(t), "runtime.json")
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			parsed, err := readServeConfiguration(path)
			if err != nil || parsed.Runtime.ClaudeCode.ManagedPolicyMode != mode {
				t.Fatal("operator mode lost during strict configuration decoding", err)
			}
			err = preflightClaudeLauncher(t.Context(), parsed)
			if mode == claudecode.ManagedPolicyTrust {
				if err != nil {
					t.Fatal("launcher did not honor explicit organization trust", err)
				}
			} else {
				var typed *claudecode.Error
				want := "policy_isolation_unavailable"
				if mode == "private-mode-sensitive" {
					want = "invalid_managed_policy_mode"
				}
				if !errors.As(err, &typed) || typed.Code != want || strings.Contains(err.Error(), "sensitive") {
					t.Fatal("launcher changed fail-closed configuration behavior", err)
				}
			}
			if len(f.Calls(t)) != 0 {
				t.Fatal("launcher preflight performed inference")
			}
		})
	}
}

func TestClaudeManagedPolicyModeIsImmutableResumeIdentity(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	config.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	c, store, _ := startInteractiveHost(t, config)
	v, err := c.Open(t.Context(), host.Create, "trust-identity")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Stop(t.Context(), "trust-identity", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	inspectInteractive(t, c, "trust-identity", func(v host.View) bool { return !v.Running })
	changed := config
	claude := *config.Runtime.ClaudeCode
	claude.ManagedPolicyMode = claudecode.ManagedPolicyReject
	changed.Runtime.ClaudeCode = &claude
	c2, _, _ := startInteractiveHost(t, changed, "--session-directory", store)
	_, err = c2.Open(t.Context(), host.Resume, "trust-identity")
	var remote *gateway.Error
	if !errors.As(err, &remote) || remote.Code != "request_failed" {
		t.Fatal("resume silently changed the trusted administrative boundary", err)
	}
	// The gateway intentionally hides backend details. Verify the underlying
	// canonical mismatch too, without weakening that safe public error contract.
	registry, err := runtimeProviders(changed.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	factory, identity, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: changed.Runtime, Providers: registry, SessionDirectory: store})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := host.New(t.Context(), host.Config{Directory: store, Build: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err = owner.Resume(t.Context(), host.Options{ID: "trust-identity", Configuration: identity}); err == nil || !strings.Contains(err.Error(), "configuration mismatch") {
		t.Fatal("canonical Host permitted policy-boundary drift", err)
	}
	if _, err = c2.Inspect(t.Context(), "trust-identity", 0, 256); !errors.As(err, &remote) || remote.Code != "not_found" {
		t.Fatal("failed resume registered a Session in the replacement Host", err)
	}
	current, err := c.Inspect(t.Context(), "trust-identity", 0, 256)
	if err != nil || current.Running {
		t.Fatal("failed resume left a running Session", err)
	}
	for _, item := range current.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == "configuration" && sessionstore.ManagedPolicyModeFromConfiguration(record.Configuration) != "trust" {
			t.Fatal("failed resume rewrote original trust configuration")
		}
	}
	if len(f.Calls(t)) != 0 {
		t.Fatal("resume/identity validation performed speculative inference")
	}
}

func TestClaudeChildOwnershipAndPermissionBoundaries(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	registry, e := runtimeProviders(config.Runtime)
	if e != nil {
		t.Fatal(e)
	}
	c := agentrunner.RuntimeConfig{Identity: config.Runtime, Providers: registry, SessionDirectory: t.TempDir()}
	_, _, e = withParentSubagents(c, map[string]childTemplate{"child": {Runtime: config.Runtime}}, permission.Unrestricted(), "", "")
	if e == nil || !strings.Contains(e.Error(), "text-only") {
		t.Fatal("Claude parent silently enabled subagent tools", e)
	}
	parent := codexServeConfiguration(t.TempDir(), "https://chatgpt.com/backend-api/codex").Runtime
	c.Identity = parent
	_, _, e = withParentSubagents(c, map[string]childTemplate{"child": {Runtime: config.Runtime}}, permission.Unrestricted(), "", "")
	if e == nil || !strings.Contains(e.Error(), "Finish") {
		t.Fatal("Claude child bypassed canonical Finish ownership", e)
	}
	cli, e := claudecode.NewClient(*config.Runtime.ClaudeCode)
	if e != nil {
		t.Fatal(e)
	}
	e = cli.Probe(permission.WithPolicy(t.Context(), permission.DenyAll()))
	if denial := permission.Failure(e); denial == nil || denial.Capability != "process" {
		t.Fatal("provider bypassed existing process permission", e)
	}
}

func TestClaudeHostShutdownDrainsOwnedSubprocess(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	f.Set(t, testclaude.Config{Wait: true, Child: true})
	registry, e := runtimeProviders(config.Runtime)
	if e != nil {
		t.Fatal(e)
	}
	factory, identity, e := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: config.Runtime, Providers: registry, SessionDirectory: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	owner, e := host.New(t.Context(), host.Config{Directory: t.TempDir(), Build: factory})
	if e != nil {
		t.Fatal(e)
	}
	defer owner.Close()
	s, e := owner.Create(t.Context(), host.Options{ID: "shutdown", Configuration: identity, Policy: permission.Unrestricted()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Submit(t.Context(), s.Generation, inbox.Input{ID: "wait", Kind: inbox.InputExternal, Payload: []byte(`"wait"`)}); e != nil {
		t.Fatal(e)
	}
	waitInteractive(t, func() bool { return len(f.Calls(t)) == 1 })
	call := f.Calls(t)[0]
	done := make(chan error, 1)
	go func() { done <- owner.Close() }()
	select {
	case e = <-done:
		if e != nil && !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Host shutdown left a Claude process/pipe")
	}
	for _, pid := range []int{call.PID, call.ChildPID} {
		if pid != 0 {
			if e = syscall.Kill(pid, 0); !errors.Is(e, syscall.ESRCH) {
				t.Fatal("Host shutdown left an unreaped process", pid, e)
			}
		}
	}
}

func TestClaudeToolShapedStreamCannotReachOperations(t *testing.T) {
	f := testclaude.New(t)
	config := claudeServeConfiguration(t, f)
	f.Set(t, testclaude.Config{Stream: `{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"permissionMode":"default","mcp_servers":[]}` + "\n" + `{"type":"assistant","message":{"id":"msg_bad","model":"claude-configured-a","content":[{"type":"tool_use","id":"tool_bad","name":"Bash","input":{"command":"touch executed"}}]}}` + "\n"})
	c, _, _ := startInteractiveHost(t, config)
	v, e := c.Open(t.Context(), host.Create, "tool-rejection")
	if e != nil {
		t.Fatal(e)
	}
	submitInteractive(t, c, v, "tool-request", "try a tool")
	failed := inspectInteractive(t, c, "tool-rejection", func(v host.View) bool { return !v.Running && v.Failure != "" })
	if !strings.Contains(failed.Failure, "tools unsupported") {
		t.Fatal("lost explicit capability failure", failed.Failure)
	}
	for _, item := range failed.History.Items {
		if item.Kind == sessionstore.ItemToolCallStatus || item.Kind == sessionstore.ItemModelResponse {
			t.Fatal("unsupported Claude tool entered canonical execution")
		}
	}
	if _, e = os.Stat(filepath.Join(config.Runtime.Workspace, "executed")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("Claude tool executed outside Operation ownership", e)
	}
}

func TestClaudeInitDiagnosticReachesGatewayWithoutRawData(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", Stream: `{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"permissionMode":"default","mcp_servers":[{"name":"server-sensitive","status":"connected"}],"agents":[],"skills":[],"plugins":[{"name":"plugin-sensitive","path":"/plugin-sensitive","body":"token-sensitive"}],"email":"email-sensitive","orgId":"org-sensitive","session_id":"session-sensitive","thinking":"reasoning-sensitive"}` + "\n"})
	config := claudeServeConfiguration(t, f)
	// The shared fixture intentionally persists its system-prompt sentinel in
	// configuration. Keep this fixture distinct from the poisoned stream data.
	config.Runtime.SystemPrompt = "Answer the user. Tools are unavailable."
	config.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	config.Permissions.Tools = []string{}
	c, _, _ := startInteractiveHost(t, config)
	v, err := c.Open(t.Context(), host.Create, "init-diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "diagnostic-input", "offline text response")
	failed := inspectInteractive(t, c, "init-diagnostic", func(v host.View) bool { return !v.Running && v.Failure != "" })
	if !strings.Contains(failed.Failure, "init_mcp_nonempty(count=1)") || !strings.Contains(failed.Failure, "Claude stream initialization violated isolation") {
		t.Fatal("gateway lost the closed initialization reason", failed.Failure)
	}
	data, err := json.Marshal(failed.History)
	if err != nil || strings.Contains(string(data), "sensitive") || strings.Contains(failed.Failure, "sensitive") || len(failed.Operations) != 0 || responseCount(failed) != 0 {
		t.Fatal("initialization diagnostic retained raw data or created an Operation", err)
	}
	calls := f.Calls(t)
	if len(calls) != 1 {
		t.Fatal("diagnostic caused a duplicate model request")
	}
	cliArgument(t, calls[0], "--tools", "")
	cliArgument(t, calls[0], "--disallowedTools", "*")
	cliArgument(t, calls[0], "--mcp-config", `{"mcpServers":{}}`)
}

func TestClaudeTeamCatalogMetadataCompletesWithoutOperations(t *testing.T) {
	f := testclaude.New(t) // Init advertises two plugins and agent/skill catalogs.
	f.Set(t, testclaude.Config{Subscription: "team"})
	config := claudeServeConfiguration(t, f)
	config.Runtime.SystemPrompt = "Answer the user. Tools are unavailable."
	config.Runtime.ClaudeCode.ManagedPolicyMode = claudecode.ManagedPolicyTrust
	config.Permissions.Tools = []string{}
	c, _, output := startInteractiveHost(t, config)
	v, err := c.Open(t.Context(), host.Create, "catalog-metadata")
	if err != nil {
		t.Fatal(err)
	}
	submitInteractive(t, c, v, "catalog-input", "offline text response")
	complete := inspectInteractive(t, c, "catalog-metadata", func(v host.View) bool { return responseCount(v) == 1 || v.Failure != "" })
	if complete.Failure != "" || len(complete.Operations) != 0 || responseCount(complete) != 1 {
		t.Fatal("catalog metadata blocked text inference or created an Operation", complete.Failure)
	}
	for _, item := range complete.History.Items {
		if item.Kind == sessionstore.ItemToolCallStatus {
			t.Fatal("catalog metadata created a canonical tool operation")
		}
	}
	data, err := json.Marshal(complete.History)
	if err != nil || strings.Contains(string(data), "sensitive") || strings.Contains(output.String(), "sensitive") {
		t.Fatal("catalog names/paths or private stream metadata leaked", err)
	}
	calls := f.Calls(t)
	if len(calls) != 1 || !slices.Contains(calls[0].Arguments, "--safe-mode") || !slices.Contains(calls[0].Arguments, "--strict-mcp-config") {
		t.Fatal("text-only inference lost its launch isolation")
	}
	cliArgument(t, calls[0], "--tools", "")
	cliArgument(t, calls[0], "--disallowedTools", "*")
	cliArgument(t, calls[0], "--mcp-config", `{"mcpServers":{}}`)
}
