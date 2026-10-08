//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// SDK 0.3.285 SDKPermissionDeniedMessage, with synthetic values only. The
// installed 2.1.285 producer also has optional decision_reason_code.
const permissionDeniedFixture = `{"type":"system","subtype":"permission_denied","tool_name":"mcp__unreal__unreal_read","tool_use_id":"call-sensitive","decision_reason_type":"asyncAgent","decision_reason":"private reason sensitive","message":"private message sensitive\u001b[31m token-sensitive account-sensitive@example.invalid","uuid":"uuid-sensitive","session_id":"session-sensitive"}`

func TestPermissionDeniedRealShapeIsTypedAndPrivate(t *testing.T) {
	for _, line := range []string{
		permissionDeniedFixture,
		`{"session_id":"session-sensitive","message":"private message sensitive","decision_reason_type":"asyncAgent","decision_reason":"private reason sensitive","subtype":"permission_denied","type":"system"}`,
		`{"type":"system","subtype":"permission_denied","message":"private message sensitive"}`,
		`{"type":"system","subtype":"permission_denied","message":"private message sensitive","decision_reason_type":"future-sensitive","decision_reason_code":"future-sensitive","future_metadata":true}`,
	} {
		for _, beforeInit := range []bool{false, true} {
			stream := line + "\n" + goodResult
			if !beforeInit {
				stream = textOnlyInit + stream
			}
			r, err := parseStream(strings.NewReader(stream), llm.RequestOptions{})
			requireCode(t, err, "permission_denied")
			var e *Error
			if !errors.As(err, &e) || e.StreamReason() == "system_unknown_shape" || len(r.Output) != 0 {
				t.Fatal("denial became unknown or success", err)
			}
			data, marshalErr := json.Marshal(e)
			if marshalErr != nil || strings.Contains(string(data)+e.Error(), "sensitive") || strings.Contains(e.Error(), "\x1b") {
				t.Fatal("denial exposed raw/private fields", marshalErr)
			}
		}
	}
}

func TestPermissionDeniedBridgeDoesNotCallHostOrReturnFinalText(t *testing.T) {
	c, f, req := bridgeClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", BridgeEvent: permissionDeniedFixture + "\n" + permissionDeniedFixture + "\n" + goodResult})
	called := 0
	r, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
		called++
		return nil, nil
	}})
	requireCode(t, err, "bridge_permission_denied")
	var e *Error
	if !errors.As(err, &e) || e.PermissionReasonType() != "asyncAgent" || e.PermissionScope() != "unreal_tool_call" {
		t.Fatal("lost closed denial classification", err)
	}
	if called != 0 || len(r.Output) != 0 || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("denial executed work, became success, or leaked", err)
	}
}

func TestPermissionDeniedReasonsAndScopeAreClosed(t *testing.T) {
	for _, reason := range []string{"rule", "mode", "subcommandResults", "permissionPromptTool", "hook", "asyncAgent", "sandboxOverride", "workingDir", "safetyCheck", "classifier", "other", "unknown", "token-sensitive"} {
		fields := map[string]any{}
		if err := json.Unmarshal([]byte(permissionDeniedFixture), &fields); err != nil {
			t.Fatal(err)
		}
		fields["decision_reason_type"] = reason
		fields["private_extra_sensitive"] = map[string]any{"token": "token-sensitive"}
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		recognized, err := permissionDeniedEvent(data, true, map[string]bridgeTool{"mcp__unreal__unreal_read": {Name: "unreal_read"}})
		var e *Error
		want := reason
		if reason == "token-sensitive" {
			want = "unknown"
		}
		if !recognized || !errors.As(err, &e) || e.PermissionReasonType() != want || e.PermissionScope() != "unreal_tool_call" || strings.Contains(e.Error(), "sensitive") {
			t.Fatal("unsafe reason mapping", err)
		}
	}
	for _, body := range []string{
		`"tool_name":"Bash","tool_use_id":"call-sensitive"`,
		`"tool_name":"mcp__foreign__read","tool_use_id":"call-sensitive"`,
		`"tool_name":"mcp__unreal__unreal_read"`,
		`"tool_name":"mcp__unreal__unreal_read","tool_use_id":"call-sensitive","agent_id":"agent-sensitive"`,
	} {
		line := []byte(`{"type":"system","subtype":"permission_denied","message":"sensitive",` + body + `}`)
		_, err := permissionDeniedEvent(line, true, map[string]bridgeTool{"mcp__unreal__unreal_read": {Name: "unreal_read"}})
		var e *Error
		if !errors.As(err, &e) || e.PermissionScope() != "unknown" {
			t.Fatal("guessed a denial scope", err)
		}
	}
}

func TestPermissionDeniedMalformedAndUnknownEventsRemainFatal(t *testing.T) {
	for _, body := range []string{
		``, `,"message":null`, `,"message":{}`, `,"message":1`,
		`,"message":"safe","session_id":null`,
		`,"message":"safe","decision_reason_type":true`,
		`,"message":"safe","decision_reason":{}`,
		`,"message":"safe","tool_use_id":[]`,
	} {
		line := `{"type":"system","subtype":"permission_denied"` + body + "}\n"
		_, err := parseStream(strings.NewReader(textOnlyInit+line+goodResult), llm.RequestOptions{})
		requireCode(t, err, "malformed_stream")
		requireStreamReason(t, err, "permission_denied_invalid_shape")
	}
	_, err := parseStream(strings.NewReader(textOnlyInit+`{"type":"system","subtype":"future_tool_execution_sensitive","message":"token-sensitive"}`+"\n"+goodResult), llm.RequestOptions{})
	requireStreamReason(t, err, "system_unknown_shape")
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatal("unknown subtype leaked", err)
	}
}
