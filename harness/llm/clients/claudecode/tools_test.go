//go:build linux || darwin

package claudecode

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestToolSchemasRejectedBeforeAnyCLIInvocation(t *testing.T) {
	c, f := fakeClient(t)
	r := textRequest()
	r.Tools = []llm.Tool{{Name: "private-tool-name-sensitive", Description: "private-tool-schema-sensitive"}}
	response, err := c.Respond(t.Context(), r, llm.RequestOptions{})
	requireCode(t, err, "tools_unsupported")
	if !strings.Contains(err.Error(), "request included tool schemas") || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("tool schema rejection lost its safe request diagnostic", err)
	}
	if len(response.Output) != 0 || len(f.Calls(t)) != 0 || len(f.AuthCalls(t)) != 0 {
		t.Fatal("tool schema rejection launched Claude or returned model output")
	}
}

func TestToolStreamRejectionsKeepDistinctNonsecretDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, stream, diagnostic string }{
		{"init", `{"type":"system","subtype":"init","tools":["private-tool-sensitive"]}` + "\n", "Claude stream initialization violated isolation"},
		{"output", initRecord + `{"type":"assistant","message":{"id":"msg_tool","model":"claude-configured-a","content":[{"type":"tool_use","name":"private-tool-sensitive","input":{"command":"private-command-sensitive"}}]}}` + "\n", "Claude stream contained non-text tool output"},
		{"delta", initRecord + `{"type":"stream_event","event":{"content_block":{"type":"tool_use","name":"private-tool-sensitive"}}}` + "\n", "Claude stream contained non-text tool output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := parseStream(strings.NewReader(tc.stream), llm.RequestOptions{})
			requireCode(t, err, "tools_unsupported")
			if len(response.Output) != 0 || !strings.Contains(err.Error(), tc.diagnostic) || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "request included") {
				t.Fatal("stream tool rejection lost its safe boundary diagnostic", err)
			}
		})
	}
}
