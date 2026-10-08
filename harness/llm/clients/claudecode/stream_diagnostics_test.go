package claudecode

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func requireStreamReason(t *testing.T, err error, want string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.StreamReason() != want || !strings.HasPrefix(e.Error(), want+": ") && !strings.HasPrefix(e.Error(), want+"(count=") {
		t.Fatalf("error=%v; want closed reason=%s", err, want)
	}
	return e
}

func TestStreamInitializationClosedReasons(t *testing.T) {
	for _, tc := range []struct {
		name, stream, code, reason string
		count                      int
	}{
		{"duplicate", textOnlyInit + textOnlyInit, "tools_unsupported", "init_duplicate", 0},
		{"tools", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":["private-tool-sensitive","second-tool-sensitive"]`, 1), "tools_unsupported", "init_tools_nonempty", 2},
		{"MCP", strings.Replace(textOnlyInit, `"mcp_servers":[]`, `"mcp_servers":[{"name":"private-server-sensitive","config":"private-config-sensitive"}]`, 1), "tools_unsupported", "init_mcp_nonempty", 1},
		{"permission", strings.Replace(textOnlyInit, `"permissionMode":"default"`, `"permissionMode":"private-mode-sensitive"`, 1), "tools_unsupported", "init_permission_mode", 0},
		{"empty mode", strings.Replace(textOnlyInit, `"permissionMode":"default"`, `"permissionMode":""`, 1), "malformed_stream", "init_permission_mode", 0},
		{"wrong tools type", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":{"private-key-sensitive":"private-value-sensitive"}`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong tool element", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":[{"private-key-sensitive":"private-value-sensitive"}]`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong MCP type", strings.Replace(textOnlyInit, `"mcp_servers":[]`, `"mcp_servers":"private-sensitive"`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong plugins type", strings.Replace(textOnlyInit, `"plugins":[]`, `"plugins":{"private-key-sensitive":"private-sensitive"}`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong mode type", strings.Replace(textOnlyInit, `"permissionMode":"default"`, `"permissionMode":{"private-key-sensitive":"private-sensitive"}`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong model type", strings.Replace(textOnlyInit, `"model":"claude-configured-a"`, `"model":{"private-key-sensitive":"private-sensitive"}`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong agents type", strings.Replace(textOnlyInit, `"agents":[]`, `"agents":{"prompt":"private-agent-prompt-sensitive"}`, 1), "malformed_stream", "init_agents_invalid", 0},
		{"wrong agent element", strings.Replace(textOnlyInit, `"agents":[]`, `"agents":[{"prompt":"private-agent-prompt-sensitive"}]`, 1), "malformed_stream", "init_agents_invalid", 0},
		{"wrong skills type", strings.Replace(textOnlyInit, `"skills":[]`, `"skills":{"body":"private-skill-body-sensitive"}`, 1), "malformed_stream", "init_skills_invalid", 0},
		{"wrong skill element", strings.Replace(textOnlyInit, `"skills":[]`, `"skills":[{"body":"private-skill-body-sensitive"}]`, 1), "malformed_stream", "init_skills_invalid", 0},
		{"invalid model", strings.Replace(textOnlyInit, `"model":"claude-configured-a"`, `"model":"private-model-sensitive\u001b"`, 1), "malformed_stream", "init_model_invalid", 0},
		{"malformed JSON", `{"type":"system","subtype":"init","private-key-sensitive":`, "malformed_stream", "init_unknown_shape", 0},
		{"duplicate JSON key", strings.Replace(textOnlyInit, `"tools":[]`, `"tools":[],"tools":["private-sensitive"]`, 1), "malformed_stream", "init_unknown_shape", 0},
		{"null root", "null\n", "malformed_stream", "init_unknown_shape", 0},
		{"wrong root type", "[]\n", "malformed_stream", "init_unknown_shape", 0},
		{"empty root", "{}\n", "malformed_stream", "init_unknown_shape", 0},
		{"wrong type field", strings.Replace(textOnlyInit, `"type":"system"`, `"type":123`, 1), "malformed_stream", "init_wrong_field_type", 0},
		{"wrong subtype field", strings.Replace(textOnlyInit, `"subtype":"init"`, `"subtype":123`, 1), "malformed_stream", "init_wrong_field_type", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Poison unrelated fields too; diagnostics must retain none of them.
			s := strings.Replace(tc.stream, `"type":"system"`, `"type":"system","prompt":"private-prompt-sensitive","email":"email-sensitive","orgId":"org-sensitive","credential":"token-sensitive","session_id":"session-sensitive","thinking":"reasoning-sensitive"`, 1)
			r, err := parseStream(strings.NewReader(s+goodResult), llm.RequestOptions{})
			requireCode(t, err, tc.code)
			e := requireStreamReason(t, err, tc.reason)
			if e.streamCount != tc.count || len(r.Output) != 0 {
				t.Fatal("rejection lost count or returned output")
			}
			encoded, marshalErr := json.Marshal(e)
			if marshalErr != nil || strings.Contains(string(encoded), "sensitive") || strings.Contains(e.Error(), "sensitive") {
				t.Fatal("diagnostic retained raw or private data")
			}
		})
	}
	for _, field := range []string{`"model":"claude-configured-a"`, `"tools":[]`, `"mcp_servers":[]`, `"permissionMode":"default"`} {
		for _, tc := range []struct{ name, replacement, reason string }{
			{"missing", "", "init_missing_required_field"},
			{"null", strings.SplitN(field, ":", 2)[0] + ":null,", "init_null_required_field"},
		} {
			t.Run(tc.name+field, func(t *testing.T) {
				s := strings.Replace(textOnlyInit, field+",", tc.replacement, 1)
				_, err := parseStream(strings.NewReader(s+goodResult), llm.RequestOptions{})
				requireCode(t, err, "malformed_stream")
				requireStreamReason(t, err, tc.reason)
			})
		}
	}
}

func TestStreamStatusClosedReasons(t *testing.T) {
	for _, tc := range []struct{ name, stream, code, reason string }{
		{"before init", `{"type":"system","subtype":"status","status":null}` + "\n", "malformed_stream", "status_before_init"},
		{"missing value", textOnlyInit + `{"type":"system","subtype":"status"}` + "\n", "malformed_stream", "status_missing_required_field"},
		{"wrong value type", textOnlyInit + `{"type":"system","subtype":"status","status":{"private-key-sensitive":true}}` + "\n", "malformed_stream", "status_wrong_field_type"},
		{"unknown value", textOnlyInit + `{"type":"system","subtype":"status","status":"private-value-sensitive"}` + "\n", "malformed_stream", "status_unknown_value"},
		{"wrong mode type", textOnlyInit + `{"type":"system","subtype":"status","status":null,"permissionMode":123}` + "\n", "malformed_stream", "status_wrong_field_type"},
		{"null mode", textOnlyInit + `{"type":"system","subtype":"status","status":null,"permissionMode":null}` + "\n", "malformed_stream", "status_permission_mode"},
		{"unsupported mode", textOnlyInit + `{"type":"system","subtype":"status","status":null,"permissionMode":"bypassPermissions"}` + "\n", "tools_unsupported", "status_permission_mode"},
		{"decode failure", textOnlyInit + `{"type":"system","subtype":"status","tools":123}` + "\n", "malformed_stream", "status_wrong_field_type"},
		{"tools", textOnlyInit + `{"type":"system","subtype":"status","tools":["private-sensitive"]}` + "\n", "tools_unsupported", "status_tools_nonempty"},
		{"MCP", textOnlyInit + `{"type":"system","subtype":"status","mcp_servers":[{"private-sensitive":true}]}` + "\n", "tools_unsupported", "status_mcp_nonempty"},
		{"wrong plugin catalog type", textOnlyInit + `{"type":"system","subtype":"status","plugins":{"private-sensitive":true}}` + "\n", "malformed_stream", "status_wrong_field_type"},
		{"unknown system", textOnlyInit + `{"type":"system","subtype":"private-sensitive"}` + "\n", "malformed_stream", "system_unknown_shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseStream(strings.NewReader(tc.stream+goodResult), llm.RequestOptions{})
			requireCode(t, err, tc.code)
			e := requireStreamReason(t, err, tc.reason)
			if strings.Contains(e.Error(), "sensitive") || strings.Contains(e.Error(), "bypassPermissions") {
				t.Fatal("status diagnostic exposed raw metadata")
			}
		})
	}
}

func TestStreamReasonEnumIsClosed(t *testing.T) {
	for n := 0; n <= 255; n++ {
		e := &Error{Code: "malformed_stream", streamReason: streamFailureReason(n)}
		if n == 0 || n > int(streamToolExecution) {
			if e.StreamReason() != "" || e.Error() != "Claude Code returned an invalid or incomplete stream" {
				t.Fatal("unknown enum exposed a fabricated reason")
			}
		} else if e.StreamReason() == "" {
			t.Fatal("declared reason has no fixed diagnostic")
		}
	}
}
