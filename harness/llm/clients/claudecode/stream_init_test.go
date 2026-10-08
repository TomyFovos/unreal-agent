package claudecode

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Keep the public initialization fields in the shape emitted by CLI 2.1.285.
// Catalog names are synthetic; no real login, policy or transcript is recorded.
const textOnlyInit = `{"type":"system","subtype":"init","model":"claude-configured-a","tools":[],"permissionMode":"default","mcp_servers":[],"agents":[],"skills":[],"plugins":[]}` + "\n"

// Public SDKSystemMessage metadata only; these entries provide no execution
// state, tool schema or permission. Values are synthetic privacy sentinels.
const pluginCatalog = `[{"name":"catalog-plugin-sensitive","path":"/catalog-plugin-sensitive","version":"catalog-version-sensitive"},{"name":"catalog-plugin-2-sensitive","path":"/catalog-plugin-2-sensitive"}]`

func TestStreamInitializationDefaultPermissionMode(t *testing.T) {
	allCatalogs := strings.Replace(textOnlyInit, `"agents":[]`, `"agents":["catalog-agent-sensitive"]`, 1)
	allCatalogs = strings.Replace(allCatalogs, `"skills":[]`, `"skills":["catalog-skill-sensitive"]`, 1)
	allCatalogs = strings.Replace(allCatalogs, `"plugins":[]`, `"plugins":`+pluginCatalog, 1)
	for _, tc := range []struct{ name, init string }{
		{"empty catalogs", textOnlyInit},
		{"agent catalog", strings.Replace(textOnlyInit, `"agents":[]`, `"agents":["catalog-agent-sensitive"]`, 1)},
		{"skill catalog", strings.Replace(textOnlyInit, `"skills":[]`, `"skills":["catalog-skill-sensitive"]`, 1)},
		{"plugin catalog", strings.Replace(textOnlyInit, `"plugins":[]`, `"plugins":`+pluginCatalog, 1)},
		{"all catalogs", allCatalogs},
		{"status catalogs", textOnlyInit + `{"type":"system","subtype":"status","status":null,"permissionMode":"default","agents":["catalog-agent-sensitive"],"skills":["catalog-skill-sensitive"],"plugins":` + pluginCatalog + `}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseStream(strings.NewReader(tc.init+goodResult), llm.RequestOptions{})
			if err != nil || len(r.Output) != 1 || r.Output[0].Data.(llm.Message).Text != "hello" {
				t.Fatal("text-only initialization rejected", err)
			}
			data, err := json.Marshal(r)
			if err != nil || strings.Contains(string(data), "catalog-") || len(r.Output) != 1 {
				t.Fatal("initialization catalog entered model output", err)
			}
		})
	}
}

func TestStreamInitializationStatusAndRateMetadata(t *testing.T) {
	for _, status := range []string{`null`, `"requesting"`, `"compacting"`} {
		t.Run(status, func(t *testing.T) {
			s := textOnlyInit + `{"type":"system","subtype":"status","status":` + status + `,"permissionMode":"default","session_id":"internal-session-sensitive","compact_error":"private-status-sensitive"}` + "\n" +
				`{"type":"stream_event","event":{"type":"message_start","message":{"content":[]}}}` + "\n" +
				`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text","text":""}}}` + "\n" +
				`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}` + "\n" +
				`{"type":"stream_event","event":{"type":"content_block_stop"}}` + "\n" +
				`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}` + "\n" +
				`{"type":"stream_event","event":{"type":"message_stop"}}` + "\n" +
				`{"type":"assistant","message":{"id":"msg_text","model":"claude-configured-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}}` + "\n" +
				`{"type":"system","subtype":"status","status":null}` + "\n" +
				`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}` + "\n" + goodResult
			var draft string
			r, err := parseStream(strings.NewReader(s), llm.RequestOptions{Progress: func(p llm.Progress) { draft += p.Delta }})
			if err != nil || draft != "hello" || len(r.Output) != 1 || r.Model != "claude-configured-a" || r.Usage.InputTokens != 60 {
				t.Fatal("compatible initialization/status stream failed", err, draft)
			}
			data, err := json.Marshal(r)
			if err != nil || strings.Contains(string(data), "sensitive") || strings.Contains(draft, "sensitive") {
				t.Fatal("status metadata leaked", err)
			}
		})
	}
}

func TestStreamInitializationRejectsMalformedAndIsolationChanges(t *testing.T) {
	for _, tc := range []struct{ name, stream, code string }{
		{"missing model", strings.Replace(textOnlyInit, `"model":"claude-configured-a",`, "", 1) + goodResult, "malformed_stream"},
		{"missing tools", strings.Replace(textOnlyInit, `"tools":[],`, "", 1) + goodResult, "malformed_stream"},
		{"null tools", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":null`, 1) + goodResult, "malformed_stream"},
		{"wrong tools type", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":{}`, 1) + goodResult, "malformed_stream"},
		{"missing MCP", strings.Replace(textOnlyInit, `"mcp_servers":[],`, "", 1) + goodResult, "malformed_stream"},
		{"null MCP", strings.Replace(textOnlyInit, `"mcp_servers":[]`, `"mcp_servers":null`, 1) + goodResult, "malformed_stream"},
		{"missing mode", strings.Replace(textOnlyInit, `"permissionMode":"default",`, "", 1) + goodResult, "malformed_stream"},
		{"null mode", strings.Replace(textOnlyInit, `"permissionMode":"default"`, `"permissionMode":null`, 1) + goodResult, "malformed_stream"},
		{"wrong mode type", strings.Replace(textOnlyInit, `"permissionMode":"default"`, `"permissionMode":true`, 1) + goodResult, "malformed_stream"},
		{"duplicate init", textOnlyInit + textOnlyInit + goodResult, "tools_unsupported"},
		{"nonempty tools", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":["Bash"]`, 1) + goodResult, "tools_unsupported"},
		{"nonempty MCP", strings.Replace(textOnlyInit, `"mcp_servers":[]`, `"mcp_servers":[{"name":"private-server-sensitive","status":"connected"}]`, 1) + goodResult, "tools_unsupported"},
		{"wrong plugin catalog type", strings.Replace(textOnlyInit, `"plugins":[]`, `"plugins":{"name":"private-plugin-sensitive"}`, 1) + goodResult, "malformed_stream"},
		{"status before init", `{"type":"system","subtype":"status","status":null}` + "\n" + textOnlyInit + goodResult, "malformed_stream"},
		{"status without value", textOnlyInit + `{"type":"system","subtype":"status"}` + "\n" + goodResult, "malformed_stream"},
		{"invalid status", textOnlyInit + `{"type":"system","subtype":"status","status":"unknown-sensitive"}` + "\n" + goodResult, "malformed_stream"},
		{"wrong status type", textOnlyInit + `{"type":"system","subtype":"status","status":{}}` + "\n" + goodResult, "malformed_stream"},
		{"unknown system event", textOnlyInit + `{"type":"system","subtype":"unknown-sensitive"}` + "\n" + goodResult, "malformed_stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseStream(strings.NewReader(tc.stream), llm.RequestOptions{})
			requireCode(t, err, tc.code)
			if len(r.Output) != 0 || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("invalid initialization returned output or leaked metadata", err)
			}
			if tc.code == "tools_unsupported" && !strings.Contains(err.Error(), "Claude stream initialization violated isolation") {
				t.Fatal("isolation boundary diagnostic changed", err)
			}
		})
	}
	for _, mode := range []string{"bypassPermissions", "acceptEdits", "auto", "plan", "dontAsk", "unknown-sensitive"} {
		t.Run(mode, func(t *testing.T) {
			for _, s := range []string{
				strings.Replace(textOnlyInit, `"permissionMode":"default"`, `"permissionMode":"`+mode+`"`, 1) + goodResult,
				textOnlyInit + `{"type":"system","subtype":"status","status":null,"permissionMode":"` + mode + `"}` + "\n" + goodResult,
			} {
				_, err := parseStream(strings.NewReader(s), llm.RequestOptions{})
				requireCode(t, err, "tools_unsupported")
				if !strings.Contains(err.Error(), "Claude stream initialization violated isolation") || strings.Contains(err.Error(), "sensitive") {
					t.Fatal("unsafe permission mode lost its nonsecret diagnostic", err)
				}
			}
		})
	}
}

func TestStreamActualToolAndAgentEventsAlwaysRejected(t *testing.T) {
	for _, event := range []string{
		`{"type":"assistant","message":{"id":"msg_tool","model":"claude-configured-a","content":[{"type":"tool_use","name":"Bash","input":{"command":"private-command-sensitive"}}]}}`,
		`{"type":"assistant","message":{"id":"msg_tool","model":"claude-configured-a","content":[{"type":"server_tool_use","name":"web_search"}]}}`,
		`{"type":"assistant","message":{"id":"msg_tool","model":"claude-configured-a","content":[{"type":"mcp_tool_use","name":"private-mcp-sensitive"}]}}`,
		`{"type":"assistant","message":{"id":"msg_tool","model":"claude-configured-a","content":[{"type":"tool_use","name":"mcp__private__sensitive","input":{}}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"server_tool_use"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"mcp_tool_use"}}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"content":[{"type":"tool_use","name":"Bash"}]}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"private-tool-sensitive"}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"}}}`,
		`{"type":"assistant","parent_tool_use_id":"private-parent-sensitive","message":{"id":"msg_child","model":"claude-configured-a","content":[{"type":"text","text":"private-child-sensitive"}]}}`,
		`{"type":"assistant","subagent_type":"private-agent-sensitive","message":{"id":"msg_child","model":"claude-configured-a","content":[{"type":"text","text":"private-child-sensitive"}]}}`,
		`{"type":"assistant","message":{"id":"msg_tool","model":"claude-configured-a","content":[],"stop_reason":"tool_use"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"private-tool-sensitive"}]},"tool_use_result":{"private-result-sensitive":true}}`,
		`{"type":"tool_progress","tool_use_id":"private-tool-sensitive","tool_name":"Bash"}`,
		`{"type":"tool_use_summary","summary":"private-tool-sensitive"}`,
		`{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"private-agent-sensitive"}`,
		`{"type":"system","subtype":"task_progress","task_id":"private-agent-sensitive"}`,
		`{"type":"system","subtype":"task_updated","task_id":"private-agent-sensitive","patch":{"status":"running"}}`,
		`{"type":"system","subtype":"task_notification","task_id":"private-agent-sensitive","status":"completed"}`,
	} {
		t.Run(event, func(t *testing.T) {
			for _, init := range []string{textOnlyInit, strings.Replace(textOnlyInit, `"plugins":[]`, `"plugins":`+pluginCatalog, 1)} {
				r, err := parseStream(strings.NewReader(init+event+"\n"+goodResult), llm.RequestOptions{})
				requireCode(t, err, "tools_unsupported")
				requireStreamReason(t, err, "stream_tool_execution")
				if len(r.Output) != 0 || !strings.Contains(err.Error(), "Claude stream contained non-text tool output") || strings.Contains(err.Error(), "sensitive") {
					t.Fatal("actual tool output passed or lost its safe diagnostic", err)
				}
			}
		})
	}
}
