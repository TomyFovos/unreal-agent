//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func normalizedProbeCall(t *testing.T, call testclaude.Call) testclaude.Call {
	t.Helper()
	call.Arguments = slices.Clone(call.Arguments)
	probeSystemPath(t, call)
	i := slices.Index(call.Arguments, "--system-prompt-file")
	// Only process identity and the random, already-removed temporary path are
	// volatile. Never normalize schema, prompt, init IDs, env or session options.
	call.Arguments[i+1] = "/private-temp/system.txt"
	call.Directory, call.PID, call.ChildPID = "/private-temp", 0, 0
	// The fake remarshal of its parsed schema has unordered map keys. Compare
	// its semantic projection too, while Initialize preserves exact wire bytes.
	var schema any
	if json.Unmarshal([]byte(call.Schema), &schema) != nil {
		t.Fatal("fake CLI did not receive a schema")
	}
	encoded, err := json.Marshal(schema, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	call.Schema = string(encoded)
	return call
}

// getcwd records the physical directory, while argv can retain a TMPDIR alias
// (notably /var -> /private/var on macOS). Validate directory identity and keep
// the exact argv path when checking the independently built wire request.
func probeSystemPath(t *testing.T, call testclaude.Call) string {
	t.Helper()
	path, err := probePromptPath(call)
	if err != nil {
		t.Fatal("unexpected system prompt transport")
	}
	return path
}

func probePromptPath(call testclaude.Call) (string, error) {
	i := slices.Index(call.Arguments, "--system-prompt-file")
	if i < 0 || i+1 >= len(call.Arguments) {
		return "", errors.New("missing prompt path")
	}
	path := call.Arguments[i+1]
	if !filepath.IsAbs(path) || filepath.Base(path) != "system.txt" || !filepath.IsAbs(call.Directory) {
		return "", errors.New("unexpected prompt path")
	}
	parent, err := probePhysicalPath(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	directory, err := probePhysicalPath(call.Directory)
	if err != nil || parent != directory {
		return "", errors.New("prompt outside generation directory")
	}
	return path, nil
}

func probePhysicalPath(path string) (string, error) {
	// Cleanup has removed the random leaf. Resolve its surviving ancestors,
	// then append the absent suffix without changing any request metadata.
	var missing []string
	for {
		physical, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				physical = filepath.Join(physical, missing[i])
			}
			return physical, nil
		}
		parent := filepath.Dir(path)
		if !os.IsNotExist(err) || parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}

func TestStructuredLayerProbePromptPathsPreserveWireAndValidateAliases(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	// Both suffixes are absent, as after generation cleanup.
	directory := filepath.Join(physical, "removed-generation")
	wirePath := filepath.Join(alias, "removed-generation", "system.txt")
	call := testclaude.Call{Directory: directory, Arguments: []string{"--system-prompt-file", wirePath}}
	path, err := probePromptPath(call)
	if err != nil || path != wirePath {
		t.Fatal("valid temp alias rejected or wire path changed", err)
	}
	for _, args := range [][]string{nil, {"--system-prompt-file"}, {"--system-prompt-file", "system.txt"}, {"--system-prompt-file", filepath.Join(alias, "other-generation", "system.txt")}, {"--system-prompt-file", filepath.Join(alias, "removed-generation", "other.txt")}} {
		invalid := call
		invalid.Arguments = args
		if _, err := probePromptPath(invalid); err == nil {
			t.Fatal("invalid system prompt transport accepted")
		}
	}
}

// This belongs to the fake-CLI gate (OfficialControl), after pure Layer units.
// It follows the exact same plan/controller as the opt-in real adapter probe,
// without invoking Respond, a tool callback, Host, Session or executor.
func TestStructuredOfficialControlProbeFreshProcessAndIdenticalPrefix(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	run := func(levels int) []structuredProbeReport {
		return runStructuredProbeLevels(t.Context(), levels, model,
			func(level int) *actionSchema { return structuredProbeSchema(t, level) },
			func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
				return c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
			})
	}
	one, five := run(1), run(5)
	if len(one) != 1 || len(five) != 5 || one[0] != five[0] {
		t.Fatal("fake CLI changed outcome between standalone/prefix probes")
	}
	for _, report := range append(slices.Clone(one), five...) {
		if !report.Diagnostic.accepted() {
			t.Fatal("fake CLI valid Final failed", report.Diagnostic.summary())
		}
	}
	calls := f.Calls(t)
	if len(calls) != 6 || len(f.CatalogCalls(t)) != 0 || len(f.StructuredProbes(t)) != 0 {
		t.Fatal("generation count, catalog or MCP/control path changed")
	}
	if !reflect.DeepEqual(normalizedProbeCall(t, calls[0]), normalizedProbeCall(t, calls[1])) {
		t.Fatal("actual levels=1 and levels=5 prefix CLI traffic differ")
	}
	seenPIDs, seenDirectories := map[int]bool{}, map[string]bool{}
	for i, call := range calls {
		if call.PID <= 0 || call.ChildPID != 0 || seenPIDs[call.PID] || seenDirectories[call.Directory] {
			t.Fatal("probe reused a subprocess or temporary directory")
		}
		seenPIDs[call.PID], seenDirectories[call.Directory] = true, true
		if !errors.Is(syscall.Kill(call.PID, 0), syscall.ESRCH) {
			t.Fatal("generation subprocess not reaped")
		}
		if _, err := os.Stat(call.Directory); !os.IsNotExist(err) {
			t.Fatal("generation temporary prompt not cleaned")
		}
		level := max(1, i)
		schema := structuredProbeSchema(t, level)
		want, err := buildStructuredRequest(model, env, schema, probeSystemPath(t, call), structuredProbeInput, false)
		if err != nil || !slices.Equal(call.Arguments, want.Arguments) || !slices.Equal(call.Environment, want.Environment) || call.Initialize != string(want.Initialize) || call.UserFrame != string(want.Input) || call.System != structuredProbeSystem || call.Input != structuredProbeInput {
			t.Fatal("actual request differs from independently tested builder contract")
		}
		for _, prohibited := range []string{"--resume", "--continue", "--session-id", "--fork-session", "--fallback-model"} {
			if slices.Contains(call.Arguments, prohibited) {
				t.Fatal("CLI request contains private continuation or fallback")
			}
		}
	}
	// A new probe on the same client also rebuilds/reaps its process after an
	// earlier failure. Its diagnostic and serializer state cannot poison a Final.
	f.Set(t, testclaude.Config{Subscription: "team", StructuredResponses: []string{"null"}, StructuredHelperInputs: []string{`{}`}})
	failed := run(5)
	if len(failed) != 1 || failed[0].Diagnostic.Outcome != probeStructuredNull || len(f.Calls(t)) != 7 {
		t.Fatal("null output did not stop at Level 1 without retry/fallback")
	}
	f.Set(t, testclaude.Config{Subscription: "team", StructuredResponses: []string{structuredProbeFinal}})
	reset := run(1)
	if len(reset) != 1 || reset[0] != one[0] || len(f.Calls(t)) != 8 {
		t.Fatal("failure/serializer state survived into a later fresh probe")
	}
	for _, call := range f.Calls(t)[6:] {
		if !errors.Is(syscall.Kill(call.PID, 0), syscall.ESRCH) {
			t.Fatal("success/failure subprocess not reaped")
		}
		if _, err := os.Stat(call.Directory); !os.IsNotExist(err) {
			t.Fatal("success/failure prompt not cleaned")
		}
	}
	t.Log("fake adapter: single Level 1 + five-level prefix PASS; 8 fresh processes reaped; exact initialize/user/argv/env/system bytes compared; MCP/Operations/execution=0")
}

func TestStructuredOfficialControlProbeValidatedActionNeverExecutes(t *testing.T) {
	c, f, _, _ := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", StructuredResponses: []string{layerRead}})
	path, env, err := c.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	reports := runStructuredProbeLevels(t.Context(), 5, llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh},
		func(level int) *actionSchema { return structuredProbeSchema(t, level) },
		func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
			calls++
			if calls < 3 {
				// Prefix fixtures are schema-validated too; only Level 3 gets the
				// fake CLI's valid read proposal, with no executor attached.
				var stdin strings.Builder
				return runStructuredProtocol(strings.NewReader(structuredProbeStream(structuredProbeFinal)), &stdin, schema, input, false)
			}
			return c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
		})
	if len(reports) != 5 || len(f.Calls(t)) != 3 {
		t.Fatal("validated Action did not complete the inert schema ladder")
	}
	for _, report := range reports[2:] {
		if report.Diagnostic.Outcome != probeValidatedAction || report.Diagnostic.Reason != probeValidationOK || !report.Diagnostic.accepted() {
			t.Fatal("validated Action was executed or mislabeled")
		}
	}
	// The adapter exposes no public ToolCall: only the validated proposal reaches
	// this test-only classifier. No action/arguments/raw response is kept there.
	encoded, _ := json.Marshal(reports)
	if strings.Contains(string(encoded), "fixture.go") || strings.Contains(string(encoded), "private-") || jsontext.Value(encoded).Kind() != '[' {
		t.Fatal("probe report retained structured action content")
	}
}

func TestStructuredOfficialControlProbeExactIndependentAndMatchesLadder(t *testing.T) {
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeFinal}})
	model := llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh}
	run := func(ladder, exact int) []structuredProbeReport {
		selected, err := selectStructuredProbeLevels(ladder, exact)
		if err != nil {
			t.Fatal(err)
		}
		// Every exact run owns a new adapter as well as new subprocess/parser
		// state. The fake machine/auth environment stays identical for comparison.
		c, err := NewClient(Config{Binary: f.Binary, Getenv: f.Getenv, ManagedPolicyMode: ManagedPolicyTrust, ToolBridge: ToolBridgeConfig{Enabled: true, Mode: BridgeModeStructured}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		path, env, err := c.prepare(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		built, generated := 0, 0
		reports := runStructuredProbeSequence(t.Context(), selected, model,
			func(level int) *actionSchema {
				if built >= len(selected) || level != selected[built] {
					t.Fatal("exact-mode visited another schema level")
				}
				built++
				return structuredProbeSchema(t, level)
			},
			func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > structuredProbeTimeout {
					t.Fatal("exact-mode changed generation timeout")
				}
				generated++
				p, response, err := c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
				if len(response.Output) != 0 {
					t.Fatal("proposal became public output or ToolCall")
				}
				return p, response, err
			})
		if built != len(selected) || generated != len(selected) || len(reports) != len(selected) {
			t.Fatal("exact-mode performed additional requests")
		}
		return reports
	}
	ladder := run(5, 0)
	for level := 1; level <= 5; level++ {
		t.Run(fmt.Sprintf("exact-%d", level), func(t *testing.T) {
			reports := run(0, level)
			if len(reports) != 1 || reports[0].Level != level || !reflect.DeepEqual(reports[0], ladder[level-1]) || !reports[0].Diagnostic.accepted() {
				t.Fatal("exact and ladder result differ")
			}
			calls := f.Calls(t)
			if len(calls) != 5+level || !reflect.DeepEqual(normalizedProbeCall(t, calls[level-1]), normalizedProbeCall(t, calls[len(calls)-1])) {
				t.Fatal("exact and ladder actual CLI requests differ")
			}
		})
	}
	seenPIDs, seenDirectories := map[int]bool{}, map[string]bool{}
	for _, call := range f.Calls(t) {
		if call.PID <= 0 || call.ChildPID != 0 || seenPIDs[call.PID] || seenDirectories[call.Directory] {
			t.Fatal("exact-mode reused a process/private request directory")
		}
		seenPIDs[call.PID], seenDirectories[call.Directory] = true, true
		if !errors.Is(syscall.Kill(call.PID, 0), syscall.ESRCH) {
			t.Fatal("exact-mode process not reaped")
		}
		if _, err := os.Stat(call.Directory); !os.IsNotExist(err) {
			t.Fatal("exact-mode private prompt not cleaned")
		}
		for _, prohibited := range []string{"--resume", "--continue", "--session-id", "--fork-session", "--fallback-model"} {
			if slices.Contains(call.Arguments, prohibited) {
				t.Fatal("exact-mode introduced continuation/fallback")
			}
		}
		argument(t, call.Arguments, "--tools", "")
		argument(t, call.Arguments, "--allowedTools", "StructuredOutput")
		argument(t, call.Arguments, "--disallowedTools", structuredDeny)
		argument(t, call.Arguments, "--mcp-config", `{"mcpServers":{}}`)
	}
	if len(seenPIDs) != 10 || len(f.AuthCalls(t)) != 6 || len(f.CatalogCalls(t)) != 0 || len(f.StructuredProbes(t)) != 0 {
		t.Fatal("exact-mode adapter independence/control isolation changed")
	}
	t.Log("exact levels 1-5 == ladder requests; 10 fresh fake processes reaped; separate adapters; execution/MCP/Operations=0")
}

func TestStructuredOfficialControlProbeExactActionProposalsOnly(t *testing.T) {
	for level := 1; level <= 5; level++ {
		t.Run(fmt.Sprintf("exact-%d", level), func(t *testing.T) {
			c, f, _, _ := structuredClient(t)
			f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, StructuredResponses: []string{structuredProbeWitness(t, level)}})
			path, env, err := c.prepare(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			selected, _ := selectStructuredProbeLevels(0, level)
			reports := runStructuredProbeSequence(t.Context(), selected, llm.Model{ID: "synthetic-opus", ReasoningEffort: llm.ReasoningEffortHigh},
				func(got int) *actionSchema {
					if got != level {
						t.Fatal("exact-mode built an earlier/later schema")
					}
					return structuredProbeSchema(t, got)
				},
				func(ctx context.Context, model llm.Model, schema *actionSchema, system, input string) (actionProposal, llm.Response, error) {
					p, response, err := c.structuredGeneration(ctx, path, env, model, schema, system, input, false)
					if len(response.Output) != 0 {
						t.Fatal("proposal became a public ToolCall/response")
					}
					return p, response, err
				})
			want := probeValidatedFinal
			if level >= 2 {
				want = probeValidatedAction
			}
			if len(reports) != 1 || reports[0].Level != level || reports[0].Diagnostic.Outcome != want || !reports[0].Diagnostic.accepted() || len(f.Calls(t)) != 1 {
				t.Fatal("exact-mode envelope misclassified or repeated")
			}
			encoded, _ := json.Marshal(reports)
			for _, private := range []string{"probe_witness", "private-", "synthetic-opus", "probe_proposal_only"} {
				if strings.Contains(string(encoded), private) || strings.Contains(reports[0].Diagnostic.summary(), private) {
					t.Fatal("probe report retained identity, arguments or private metadata")
				}
			}
		})
	}
}
