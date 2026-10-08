//go:build linux || darwin

package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

// All 13 SDKAssistantMessageError values, verified against the local pinned
// /tmp/unreal-claude-discovery-sdk/sdk.d.ts and sdk.mjs (Version: 0.3.285),
// and the installed 2.1.285 enum. Tests remain portable/offline: no live SDK,
// account, network request or credential access is required.
var sdkAssistantErrorCases = []struct{ value, reason, code string }{
	{"authentication_failed", "assistant_authentication_failed", "external_reauth_required"},
	{"oauth_org_not_allowed", "assistant_oauth_org_not_allowed", "subscription_unavailable"},
	{"account_on_hold", "assistant_account_on_hold", "subscription_unavailable"},
	{"verification_required", "assistant_verification_required", "subscription_unavailable"},
	{"billing_error", "assistant_billing_error", "subscription_unavailable"},
	{"rate_limit", "assistant_rate_limit", "rate_limited"},
	{"overloaded", "assistant_overloaded", "subprocess_failure"},
	{"invalid_request", "assistant_invalid_request", "provider_request_rejected"},
	{"model_not_found", "assistant_model_not_found", "invalid_model"},
	{"server_error", "assistant_server_error", "subprocess_failure"},
	{"unknown", "assistant_unknown", "subprocess_failure"},
	{"max_output_tokens", "assistant_max_output_tokens", "generation_output_limit"},
	{"cloud_credential_error", "assistant_cloud_credential_error", "external_reauth_required"},
}

func TestSDKAssistantErrorsAreClosedAndTyped(t *testing.T) {
	schema := diagnosticSchema(t)
	cases := append(sdkAssistantErrorCases[:len(sdkAssistantErrorCases):len(sdkAssistantErrorCases)],
		struct{ value, reason, code string }{"private-token authentication_failed invalid_request", "assistant_provider_error_unknown", "subprocess_failure"},
		struct{ value, reason, code string }{"SERVER_ERROR", "assistant_provider_error_unknown", "subprocess_failure"},
		struct{ value, reason, code string }{" invalid_request", "assistant_provider_error_unknown", "subprocess_failure"},
		struct{ value, reason, code string }{"", "assistant_provider_error_unknown", "subprocess_failure"},
	)
	for _, tc := range cases {
		t.Run(tc.reason+"/"+tc.value, func(t *testing.T) {
			frame, err := json.Marshal(map[string]any{
				"type": "assistant", "error": tc.value, "session_id": "private-session-id", "uuid": "private-helper-id",
				"message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "sensitive-prose private-token secret-account authentication_failed invalid model"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			proposal, response, err := readStructuredStream(bufio.NewScanner(strings.NewReader(diagnosticInit+string(frame)+"\n"+diagnosticFinal)), schema, nil, nil)
			e := requireStructuredStage(t, err, "structured_result_invalid")
			if e.Code != tc.code || e.StructuredDetails().AssistantReason != tc.reason || proposal.Action != nil || proposal.Final != nil || len(response.Output) != 0 {
				t.Fatal("SDK error collapsed or became a proposal", e.Code, e.StructuredDetails().AssistantReason)
			}
			encoded, _ := json.Marshal(e)
			var stored map[string]jsontext.Value
			if json.Unmarshal(encoded, &stored) != nil || len(stored) != 1 || !jsonStringEquals(stored["Code"], tc.code) || e.assistantError == assistantErrorNone {
				t.Fatal("Error stores a raw provider string")
			}
			if bytes.Contains(encoded, []byte("private-token")) {
				t.Fatal("raw non-SDK error value retained")
			}
			// The same exact classification applies to text-only SDK assistant
			// frames, while result/errors/message retain the legacy classifier.
			_, err = parseStream(strings.NewReader(strings.Replace(diagnosticInit, `["StructuredOutput"]`, `[]`, 1)+string(frame)+"\n"), llm.RequestOptions{})
			var textError *Error
			if !errors.As(err, &textError) || textError.Code != tc.code || !strings.Contains(err.Error(), "reason="+tc.reason) {
				t.Fatal("text-only assistant did not use the closed enum", err)
			}
		})
	}
}

func TestSDKAssistantErrorDoesNotClassifyFreeText(t *testing.T) {
	for _, raw := range []string{`"model_not_found malicious-private-token"`, `"rate_limit malicious-private-token"`, `{"type":"authentication_failed","message":"private-token"}`} {
		e := assistantFailure(jsontext.Value(raw))
		if e.AssistantReason() != "assistant_provider_error_unknown" || strings.Contains(e.Error(), "private-token") {
			t.Fatal("untrusted error body matched a known enum or leaked")
		}
	}
}

func TestSDKAssistantErrorsAlsoFailClosedInMCPCompatibilityMode(t *testing.T) {
	for _, tc := range []struct{ value, reason, code string }{
		{"invalid_request", "assistant_invalid_request", "provider_request_rejected"},
		{"private-token authentication_failed", "assistant_provider_error_unknown", "subprocess_failure"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			c, f, req := bridgeClient(t)
			frame, err := json.Marshal(map[string]any{"type": "assistant", "error": tc.value, "message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "private-token"}}}})
			if err != nil {
				t.Fatal(err)
			}
			f.Set(t, testclaude.Config{Subscription: "team", BridgeEvent: string(frame)})
			callbacks := 0
			response, err := c.Respond(t.Context(), req, llm.RequestOptions{Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
				callbacks++
				return nil, nil
			}})
			var e *Error
			if !errors.As(err, &e) || e.Code != tc.code || e.AssistantReason() != tc.reason || callbacks != 0 || len(response.Output) != 0 || strings.Contains(err.Error(), "private-token") {
				t.Fatal("MCP assistant error bypassed enum or reached execution", err, callbacks)
			}
		})
	}
}
