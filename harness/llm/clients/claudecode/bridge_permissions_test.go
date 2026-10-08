//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestBridgeManagedPolicyCannotBeBypassedByCLIAllowlist(t *testing.T) {
	for _, rules := range []string{
		`[]`,
		`[{"behavior":"allow","source":"cliArg","rule":"mcp__unreal__unreal_read"}]`,
		`[{"behavior":"allow","source":"policySettings","rule":"mcp__unreal__*"}]`,
		`[{"behavior":"allow","source":"policySettings","rule":"mcp__unreal__unreal_read","notInEffect":true}]`,
		`[{"behavior":"deny","source":"policySettings","rule":"mcp__unreal__unreal_read"}]`,
		`[{"behavior":"allow","source":"policySettings","rule":"mcp__foreign__read"}]`,
		`[{"behavior":"allow","source":"private-source-sensitive","rule":"mcp__unreal__unreal_read"}]`,
	} {
		c, f, req := bridgeClient(t)
		f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, BridgePermissionRules: rules})
		_, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: bridgeReply})
		requireCode(t, err, "bridge_allowlist_inactive")
		if len(f.Calls(t)) != 0 || strings.Contains(err.Error(), "sensitive") {
			t.Fatal("policy check sent user input or exposed policy", err)
		}
	}
	c, f, req := bridgeClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true})
	requireCode(t, c.ProbeToolBridge(t.Context(), req.Tools), "bridge_allowlist_inactive")
	if len(f.Calls(t)) != 0 {
		t.Fatal("permission probe sent inference")
	}
}

func TestBridgeExplicitManagedSDKToolGrantAndExactExposure(t *testing.T) {
	c, f, req := bridgeClient(t)
	names := []string{"read", "grep", "glob", "write", "edit", "Bash", "SubagentStart"}
	var rules []map[string]any
	var want []string
	req.Tools = nil
	for _, name := range names {
		req.Tools = append(req.Tools, llm.Tool{Type: llm.ToolFunction, Name: name, Parameters: map[string]any{"type": "object"}})
		full := "mcp__unreal__unreal_" + name
		want = append(want, full)
		rules = append(rules, map[string]any{"behavior": "allow", "source": "policySettings", "rule": full})
	}
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	f.Set(t, testclaude.Config{Subscription: "team", BridgeManagedPermissionsOnly: true, BridgePermissionRules: string(data), BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{}`, Contains: "canonical file receipt"}}})
	called := 0
	_, err = c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(ctx context.Context, r llm.Response) ([]llm.ToolOutcome, error) {
		called++
		return bridgeReply(ctx, r)
	}})
	if err != nil || called != 1 || len(f.Calls(t)) != 1 {
		t.Fatal("explicit managed SDK grant did not permit Unreal callback", err)
	}
	var listed struct {
		Response struct {
			Response struct {
				MCPResponse struct {
					Result struct {
						Tools []struct {
							Name string `json:"name"`
						} `json:"tools"`
					} `json:"result"`
				} `json:"mcp_response"`
			} `json:"response"`
		} `json:"response"`
	}
	data, err = os.ReadFile(filepath.Join(f.Directory, "bridge-list.json"))
	if err != nil || json.Unmarshal(data, &listed) != nil {
		t.Fatal("no SDK tools/list fixture", err)
	}
	var exposed []string
	for _, tool := range listed.Response.Response.MCPResponse.Result.Tools {
		exposed = append(exposed, "mcp__unreal__"+tool.Name)
	}
	var allowed []string
	args := f.Calls(t)[0].Arguments
	for i, arg := range args {
		if arg == "--allowedTools" {
			allowed = strings.Split(args[i+1], ",")
		}
	}
	if !slices.Equal(exposed, want) || !slices.Equal(allowed, exposed) {
		t.Fatal("exposed SDK tools != exact allowlist", exposed, allowed)
	}
	argument(t, args, "--tools", "")
	argument(t, args, "--disallowedTools", bridgeBuiltinDeny)
	argument(t, args, "--mcp-config", bridgeMCP)
	for _, name := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "Agent", "mcp__foreign__read"} {
		if slices.Contains(exposed, name) || slices.Contains(allowed, name) {
			t.Fatal("built-in or foreign tool was exposed", name)
		}
	}
}

func TestBridgePermissionRuleShapeAndLaunchAllowlistFailClosed(t *testing.T) {
	names := map[string]bridgeTool{"mcp__unreal__unreal_read": {Name: "unreal_read"}}
	for _, state := range []*bridgePermissionState{nil, {}, {ManagedOnly: new(false)}} {
		requireCode(t, validateBridgePermissions(state, names), "bridge_unavailable")
	}
	_, _, req := bridgeClient(t)
	tools, err := bridgeTools(req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"*", "mcp__*", "mcp__unreal__*", "Bash", "Read", "Edit", "Agent", "mcp__foreign__read", "mcp__unreal__unreal_read,mcp__foreign__read"} {
		args := bridgeArgs(tools)
		args[slices.Index(args, "--allowedTools")+1] = value
		requireCode(t, validateBridgeLaunch(args, tools), "isolation_contract_invalid")
	}
}
