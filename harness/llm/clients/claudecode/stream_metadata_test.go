package claudecode

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// These synthetic records follow SDK 0.3.285, not a private captured transcript.
// Nothing in these bodies may appear in conversation, progress or diagnostics.
var safeMetadata = []struct{ name, body string }{
	{"api_retry", `"attempt":1,"max_retries":3,"retry_delay_ms":100,"error_status":null,"error":"overloaded","no_response":{"waited_ms":10,"retry_wait_ms":20}`},
	{"thinking_tokens", `"estimated_tokens":11,"estimated_tokens_delta":2,"user_message_uuid":"user-identity-sensitive"`},
	{"informational", `"content":"prompt assistant token email reasoning sensitive\u001b[31m","level":"notice","prevent_continuation":false`},
	{"notification", `"key":"notification-identity-sensitive","text":"prompt assistant token email reasoning sensitive\u001b[31m","priority":"medium","timeout_ms":100,"color":"color-sensitive"`},
	{"session_state_changed", `"state":"running"`},
}

func metadataRecord(name, body string) string {
	return `{"type":"system","subtype":"` + name + `","uuid":"uuid-sensitive","session_id":"session-sensitive",` + body + "}\n"
}

func TestStreamKnownMetadataIsValidatedAndDiscarded(t *testing.T) {
	var together string
	for _, event := range safeMetadata {
		t.Run(event.name, func(t *testing.T) {
			line := metadataRecord(event.name, event.body)
			var progress []llm.Progress
			r, err := parseStream(strings.NewReader(textOnlyInit+line+goodResult), llm.RequestOptions{Progress: func(p llm.Progress) { progress = append(progress, p) }})
			if err != nil || len(r.Output) != 1 || r.Output[0].Data.(llm.Message).Text != "hello" || len(progress) != 0 || r.Usage.ReasoningTokens != 0 || !slices.Contains(r.Usage.Unknown, llm.UsageReasoning) {
				t.Fatal("metadata changed text, progress or usage", err)
			}
			data, err := json.Marshal(r)
			if err != nil || strings.Contains(string(data), "sensitive") {
				t.Fatal("system metadata leaked", err)
			}
		})
		together += metadataRecord(event.name, event.body)
	}
	if _, err := parseStream(strings.NewReader(textOnlyInit+together+goodResult), llm.RequestOptions{}); err != nil {
		t.Fatal("combined metadata stream failed", err)
	}
	if _, err := parseStream(strings.NewReader(textOnlyInit+metadataRecord("session_state_changed", `"state":"idle"`)+goodResult), llm.RequestOptions{}); err != nil {
		t.Fatal("non-owning idle notification was rejected", err)
	}
}

func TestStreamMetadataMalformedShapesFailClosed(t *testing.T) {
	for _, event := range safeMetadata {
		line := metadataRecord(event.name, event.body)
		var fields map[string]jsontext.Value
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		// Every required identity/discriminant is explicitly typed. Arbitrary
		// extra fields cannot turn a supported notice into tool/control output.
		for _, tc := range []struct {
			name   string
			change func(map[string]jsontext.Value)
		}{
			{"missing uuid", func(f map[string]jsontext.Value) { delete(f, "uuid") }},
			{"null session", func(f map[string]jsontext.Value) { f["session_id"] = []byte(`null`) }},
			{"wrong subtype", func(f map[string]jsontext.Value) { f["subtype"] = []byte(`123`) }},
			{"extra field", func(f map[string]jsontext.Value) { f["private-field-sensitive"] = []byte(`"private-value-sensitive"`) }},
		} {
			t.Run(event.name+"/"+tc.name, func(t *testing.T) {
				copy := map[string]jsontext.Value{}
				for k, v := range fields {
					copy[k] = v
				}
				tc.change(copy)
				data, err := json.Marshal(copy)
				if err != nil {
					t.Fatal(err)
				}
				_, err = parseStream(strings.NewReader(textOnlyInit+string(data)+"\n"+goodResult), llm.RequestOptions{})
				requireCode(t, err, "malformed_stream")
				e := requireStreamReason(t, err, "system_unknown_shape")
				if e.SystemShape() == nil || strings.Contains(e.Error(), "sensitive") {
					t.Fatal("missing/unsafe shape", err)
				}
			})
		}
	}
	for _, line := range []string{
		metadataRecord("thinking_tokens", `"estimated_tokens":"private-sensitive","estimated_tokens_delta":1`),
		metadataRecord("api_retry", `"attempt":1,"max_retries":3,"retry_delay_ms":100,"error_status":null,"error":"private-error-sensitive"`),
		metadataRecord("api_retry", `"attempt":1,"max_retries":3,"retry_delay_ms":100,"error_status":null,"error":"overloaded","no_response":{"waited_ms":1,"retry_wait_ms":2,"tool":"private-tool-sensitive"}`),
		metadataRecord("notification", `"key":"private-sensitive","text":"private-sensitive","priority":"private-priority-sensitive"`),
		metadataRecord("informational", `"content":"private-sensitive","level":"private-level-sensitive"`),
		metadataRecord("session_state_changed", `"state":"requires_action"`),
		`{"type":"system","subtype":null}` + "\n",
		`{"type":"system"}` + "\n",
	} {
		_, err := parseStream(strings.NewReader(textOnlyInit+line+goodResult), llm.RequestOptions{})
		requireCode(t, err, "malformed_stream")
		e := requireStreamReason(t, err, "system_unknown_shape")
		if e.SystemShape() == nil || strings.Contains(e.Error(), "sensitive") {
			t.Fatal("unsafe malformed diagnostic", err)
		}
	}
	_, err := parseStream(strings.NewReader(metadataRecord("thinking_tokens", safeMetadata[1].body)+textOnlyInit+goodResult), llm.RequestOptions{})
	requireStreamReason(t, err, "system_unknown_shape")
}

func TestStreamMetadataDoesNotPermitExecutionOrStopOverrides(t *testing.T) {
	for _, line := range []string{
		metadataRecord("informational", `"content":"private-sensitive","level":"notice","tool_use_id":"tool-sensitive"`),
		metadataRecord("notification", `"key":"private-sensitive","text":"private-sensitive","priority":"medium","task_id":"task-sensitive"`),
		metadataRecord("thinking_tokens", `"estimated_tokens":1,"estimated_tokens_delta":1,"mcp_server_name":"server-sensitive"`),
	} {
		_, err := parseStream(strings.NewReader(textOnlyInit+line+goodResult), llm.RequestOptions{})
		requireCode(t, err, "tools_unsupported")
		requireStreamReason(t, err, "stream_tool_execution")
	}
	_, err := parseStream(strings.NewReader(textOnlyInit+metadataRecord("informational", `"content":"private-sensitive","level":"warning","prevent_continuation":true`)+goodResult), llm.RequestOptions{})
	requireCode(t, err, "subprocess_failure")
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatal("stop policy body leaked")
	}
	for _, subtype := range []string{"compact_boundary", "model_refusal_fallback", "control_request_progress", "hook_started", "memory_recall", "files_persisted", "plugin_install", "unknown_execution_sensitive"} {
		_, err := parseStream(strings.NewReader(textOnlyInit+metadataRecord(subtype, `"content":"private-sensitive"`)+goodResult), llm.RequestOptions{})
		requireCode(t, err, "malformed_stream")
		e := requireStreamReason(t, err, "system_unknown_shape")
		if e.SystemShape() == nil || strings.Contains(e.Error(), "sensitive") {
			t.Fatal("execution-like shape leaked", err)
		}
	}
}

func TestUnknownSystemShapeContainsOnlyClosedSchema(t *testing.T) {
	const line = `{"type":"system","subtype":"compact_boundary","uuid":"uuid-sensitive","session_id":"session-sensitive","compact_metadata":{"prompt":"private-prompt-sensitive"},"agents":[],"skills":null,"plugins":[],"is_error":true,"model":"token-sensitive","attempt":2,"private-key-sensitive":"token-sensitive","another-private-sensitive":{"reasoning":"reasoning-sensitive"}}` + "\n"
	_, err := parseStream(strings.NewReader(textOnlyInit+line+goodResult), llm.RequestOptions{})
	e := requireStreamReason(t, err, "system_unknown_shape")
	s := e.SystemShape()
	if s == nil || s.Type != "system" || s.Subtype != "compact_boundary" || !s.TypePresent || !s.SubtypePresent || !s.Object || len(s.UnknownFields) != 2 {
		t.Fatal("lost safe shape", err)
	}
	for _, expected := range []StreamField{{"compact_metadata", "object"}, {"agents", "array"}, {"skills", "null"}, {"is_error", "boolean"}, {"attempt", "number"}, {"model", "string"}} {
		if !slices.Contains(s.Fields, expected) {
			t.Fatal("missing field JSON type", expected)
		}
	}
	data, err := json.Marshal(s)
	if err != nil || strings.Contains(string(data), "sensitive") || strings.Contains(e.Error(), "sensitive") {
		t.Fatal("values/arbitrary keys leaked", err)
	}
	s.Fields[0].Name = "caller-mutation-sensitive"
	if strings.Contains(e.Error(), "caller-mutation") {
		t.Fatal("caller mutated safe error")
	}
	for _, subtype := range []string{`"private-subtype-sensitive"`, `null`, `123`} {
		_, err := parseStream(strings.NewReader(textOnlyInit+`{"type":"system","subtype":`+subtype+`,"uuid":"uuid-sensitive"}`+"\n"+goodResult), llm.RequestOptions{})
		e := requireStreamReason(t, err, "system_unknown_shape")
		if e.SystemShape().Subtype != "unknown" || strings.Contains(e.Error(), "sensitive") {
			t.Fatal("open subtype value leaked", err)
		}
	}
}

func TestInferenceRejectsDiscoveryControlFrames(t *testing.T) {
	for _, frame := range []string{
		`{"type":"control_response","response":{"subtype":"success","request_id":"control-private-sensitive","response":{"models":[],"account":{"email":"email-sensitive"}}}}`,
		`{"type":"control_request","request_id":"control-private-sensitive","request":{"subtype":"list_models"}}`,
	} {
		r, err := parseStream(strings.NewReader(textOnlyInit+frame+"\n"+goodResult), llm.RequestOptions{})
		requireCode(t, err, "malformed_stream")
		if len(r.Output) != 0 || strings.Contains(err.Error(), "sensitive") {
			t.Fatal("inference accepted/leaked control data")
		}
	}
	var commands bytes.Buffer
	_, err := readCatalogControls(strings.NewReader(textOnlyInit+goodResult), &commands)
	requireCode(t, err, "tools_unsupported")
	if strings.Contains(commands.String(), `"type":"user"`) {
		t.Fatal("discovery attempted to continue an inference")
	}
}
