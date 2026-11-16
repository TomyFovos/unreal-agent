package opencodego

import (
	"bytes"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

// The live probe records how OpenCode Go answers the Responses requests Unreal
// sends (docs/opencode-go.md, 接続検証). It runs only on request: set
// UNREAL_LIVE_OPENCODE_GO=1, store the key as opencode-go/api_key/primary and
// name an absolute UNREAL_OPENCODE_GO_CAPTURE directory for the anonymized
// captures. It never prints or stores the key.
//
// Any offered model can be probed: UNREAL_OPENCODE_GO_MODEL names it and
// defaults to probeModel; the Host and sub-agent live checks read the same
// variable. Before the first keyed request the probe reads the public model
// list, which needs no key, and takes the efforts and the default effort from
// it, so plain and the tool scenarios send the effort Unreal itself would and
// the effort scenario covers the other efforts the list names.
const (
	probeHost     = "opencode.ai"
	probeEndpoint = "https://" + probeHost + "/inference/go/openai/v1/responses"
	probeModel    = "deepseek-v4.1-flash"
)

func TestLiveOpenCodeGoResponsesProbe(t *testing.T) {
	if os.Getenv("UNREAL_LIVE_OPENCODE_GO") != "1" {
		t.Skip("set UNREAL_LIVE_OPENCODE_GO=1 to call OpenCode Go with the stored key")
	}
	captureDir := os.Getenv("UNREAL_OPENCODE_GO_CAPTURE")
	if !filepath.IsAbs(captureDir) {
		t.Fatal("UNREAL_OPENCODE_GO_CAPTURE must name an absolute directory outside the repository")
	}
	if err := os.MkdirAll(captureDir, 0700); err != nil {
		t.Fatal(err)
	}
	model := probeModelFromEnv()
	catalog := probeCatalog(t, model)
	key := storedKey(t)
	session := rand.Text()
	recorder := &probeRecorder{base: http.DefaultTransport}
	httpClient := &http.Client{Transport: recorder, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect refused")
	}}
	remote := primitives.NewRemoteClientWithHTTPClient(httpClient)
	defer remote.Close()
	// One attempt each: the probe records the provider's first answer and
	// spends no allowance on retries.
	one := 1
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: probeEndpoint,
		Headers: map[string][]string{
			"Authorization":      {"Bearer " + key},
			"Content-Type":       {"application/json"},
			"User-Agent":         {"unreal-agent"},
			"x-opencode-session": {session},
		},
		MaxAttempts:       &one,
		MaxStreamAttempts: &one,
	})
	if err != nil {
		t.Fatal(err)
	}
	anonymizer := newProbeAnonymizer(key, session)
	var results []probeResult
	run := func(name string, effort llm.ReasoningEffort, input []llm.Item, tools []llm.Tool) (llm.Response, bool) {
		first := recorder.count()
		response, err := adapter.Respond(t.Context(), llm.Request{Model: llm.Model{ID: model, ReasoningEffort: effort}, Input: input, Tools: tools}, llm.RequestOptions{})
		result := probeResult{Scenario: name, Effort: effort, Exchanges: recorder.since(first)}
		if err != nil {
			result.Error = anonymizer.text(err.Error())
			var apiErr *responsesapi.APIError
			if errors.As(err, &apiErr) {
				result.APIError = &probeAPIError{Status: apiErr.StatusCode, Code: apiErr.Code, Type: apiErr.Type, Param: apiErr.Param, Message: anonymizer.text(apiErr.Message)}
			}
			t.Errorf("%s (%s): %s", name, effort, result.Error)
		} else {
			usage := response.Usage
			usage.Raw = nil
			result.Usage = &usage
			for _, item := range response.Output {
				result.Output = append(result.Output, string(item.Type))
			}
			t.Logf("%s (%s): output %v, input %d, cached %d, output %d, reasoning %d tokens", name, effort, result.Output, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ReasoningTokens)
		}
		results = append(results, result)
		return response, err == nil
	}

	writeCapture := func() {
		exchanges := make([]probeExchange, 0, recorder.count())
		for _, raw := range recorder.since(0) {
			exchanges = append(exchanges, anonymizer.exchange(raw))
		}
		summary := probeSummary{Model: model, Endpoint: probeEndpoint, Results: results, Allowance: findAllowance(exchanges), Exchanges: exchanges}
		data, err := json.Marshal(summary, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(key)) || bytes.Contains(data, []byte(session)) {
			t.Fatal("capture still contains the key or the session value")
		}
		path := filepath.Join(captureDir, "opencode-go-probe-"+model+".json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d requests; allowance fields: %v; capture: %s", len(exchanges), summary.Allowance, path)
	}

	// A long shared prefix makes provider prompt caching measurable.
	prefix := []llm.Item{message(llm.RoleSystem, strings.Repeat("This sentence pads the shared system prompt so that the cached prefix is long enough to measure. ", 120)+"Answer tersely.")}
	plain := append(slices.Clone(prefix), message(llm.RoleUser, "Reply with the single word: ok"))
	// The default effort is empty when the model takes no effort, or when the
	// list does not name one: Unreal then sends no reasoning field at all.
	if _, ok := run("plain", catalog.DefaultEffort, plain, nil); !ok {
		writeCapture()
		return
	}
	for _, effort := range catalog.Efforts {
		if effort == catalog.DefaultEffort {
			continue
		}
		run("effort", effort, plain, nil)
	}

	tools := []llm.Tool{{Type: llm.ToolFunction, Name: "lookup_number", Description: "Return the number stored under a key.", Parameters: map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"key": map[string]any{"type": "string"}},
		"required":             []any{"key"},
		"additionalProperties": false,
	}}}
	input := append(slices.Clone(prefix), message(llm.RoleUser, "Call lookup_number with key alpha, then reply with only the number it returns."))
	called, ok := run("tool_call", catalog.DefaultEffort, input, tools)
	var call *llm.ToolCall
	for _, item := range called.Output {
		if value, isCall := item.Data.(llm.ToolCall); isCall && item.Type == llm.ItemToolCall {
			call = &value
			break
		}
	}
	if ok && call == nil {
		t.Error("tool_call: the model did not call the tool")
	}
	if call != nil {
		input = append(input, called.Output...)
		input = append(input, llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: call.CallID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "42"}}}})
		answered, answeredOK := run("tool_result_with_reasoning", catalog.DefaultEffort, input, tools)
		withoutReasoning := slices.DeleteFunc(slices.Clone(input), func(item llm.Item) bool { return item.Type == llm.ItemReasoning })
		run("tool_result_without_reasoning", catalog.DefaultEffort, withoutReasoning, tools)
		if answeredOK {
			input = append(input, answered.Output...)
			input = append(input, message(llm.RoleSystem, "From now on, answer in uppercase."), message(llm.RoleUser, "Say done."))
			steered, steeredOK := run("mid_conversation_system", catalog.DefaultEffort, input, tools)
			if steeredOK {
				input = append(input, steered.Output...)
				input = append(input, message(llm.RoleUser, "Say done again."))
				run("append_only_continuation", catalog.DefaultEffort, input, tools)
			}
		}
	}

	writeCapture()
}

func storedKey(t *testing.T) string {
	t.Helper()
	directory := os.Getenv("UNREAL_OPENCODE_GO_CREDENTIALS")
	if directory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		directory = filepath.Join(home, ".local", "state", "unreal-agent", "credentials")
	}
	store, err := credential.OpenLocal(directory)
	if err != nil {
		t.Fatal(err)
	}
	material, err := credential.NewManager(store).Resolve(t.Context(), credential.Reference{Provider: "opencode-go", ID: "primary", Method: credential.APIKey})
	if err != nil {
		t.Fatal("OpenCode Go key: ", err)
	}
	key := material.Token.Reveal()
	if strings.TrimSpace(key) == "" {
		t.Fatal("OpenCode Go key is empty")
	}
	return key
}

func message(role llm.Role, text string) llm.Item {
	return llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: role, Text: text}}
}

type probeAPIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code,omitzero"`
	Type    string `json:"type,omitzero"`
	Param   string `json:"param,omitzero"`
	Message string `json:"message,omitzero"`
}

type probeResult struct {
	Scenario  string              `json:"scenario"`
	Effort    llm.ReasoningEffort `json:"effort"`
	Exchanges []rawExchange       `json:"-"`
	Output    []string            `json:"output,omitzero"`
	Usage     *llm.Usage          `json:"usage,omitzero"`
	Error     string              `json:"error,omitzero"`
	APIError  *probeAPIError      `json:"api_error,omitzero"`
}

type probeSummary struct {
	Model     string          `json:"model"`
	Endpoint  string          `json:"endpoint"`
	Results   []probeResult   `json:"results"`
	Allowance []string        `json:"allowance"`
	Exchanges []probeExchange `json:"exchanges"`
}

// rawExchange holds one HTTP exchange before anonymization. It stays in memory.
type rawExchange struct {
	Method          string
	Path            string
	RequestHeaders  http.Header
	RequestBody     []byte
	Status          int
	ResponseHeaders http.Header
	ResponseBody    []byte
	Error           string
}

// probeRecorder refuses every destination except OpenCode Go and keeps a copy
// of each exchange. It reads the whole stream before returning it.
type probeRecorder struct {
	base      http.RoundTripper
	mu        sync.Mutex
	exchanges []rawExchange
}

func (r *probeRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != probeHost || !strings.HasPrefix(request.URL.Path, "/inference/go/") {
		return nil, fmt.Errorf("probe refuses %s://%s%s", request.URL.Scheme, request.URL.Host, request.URL.Path)
	}
	exchange := rawExchange{Method: request.Method, Path: request.URL.Path, RequestHeaders: request.Header.Clone()}
	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		_ = request.Body.Close()
		if err != nil {
			return nil, err
		}
		exchange.RequestBody = body
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	response, err := r.base.RoundTrip(request)
	if err != nil {
		exchange.Error = err.Error()
		r.add(exchange)
		return nil, err
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	exchange.Status = response.StatusCode
	exchange.ResponseHeaders = response.Header.Clone()
	exchange.ResponseBody = body
	if readErr != nil {
		exchange.Error = readErr.Error()
	}
	r.add(exchange)
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, readErr
}

func (r *probeRecorder) add(exchange rawExchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchanges = append(r.exchanges, exchange)
}

func (r *probeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exchanges)
}

func (r *probeRecorder) since(first int) []rawExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.exchanges[first:])
}

type probeExchange struct {
	Method          string              `json:"method"`
	Path            string              `json:"path"`
	RequestHeaders  map[string][]string `json:"request_headers"`
	RequestBody     any                 `json:"request_body,omitzero"`
	Status          int                 `json:"status"`
	ResponseHeaders map[string][]string `json:"response_headers,omitzero"`
	// Events holds a streamed response, one decoded data frame each; Body holds any other.
	Events []probeEvent `json:"events,omitzero"`
	Body   any          `json:"body,omitzero"`
	Error  string       `json:"error,omitzero"`
}

type probeEvent struct {
	Event string `json:"event,omitzero"`
	Type  string `json:"type,omitzero"`
	Data  any    `json:"data,omitzero"`
}

// probeAnonymizer replaces the key, the session value, identifiers and
// encrypted reasoning. The same identifier always maps to the same
// placeholder, so request and response items still line up.
type probeAnonymizer struct {
	secrets []string
	session string
	ids     map[string]string
	counts  map[string]int
}

func newProbeAnonymizer(key, session string) *probeAnonymizer {
	return &probeAnonymizer{secrets: []string{key, session}, session: session, ids: map[string]string{}, counts: map[string]int{}}
}

var probeIdentifierFields = map[string]string{
	"id":                   "id",
	"call_id":              "call",
	"item_id":              "item",
	"response_id":          "response",
	"previous_response_id": "response",
	"user":                 "user",
	"user_id":              "user",
	"account_id":           "account",
	"organization":         "organization",
	"project":              "project",
	"safety_identifier":    "user",
	"prompt_cache_key":     "cache",
}

func (a *probeAnonymizer) text(value string) string {
	for _, secret := range a.secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	return value
}

func (a *probeAnonymizer) placeholder(kind, value string) string {
	if existing, ok := a.ids[value]; ok {
		return existing
	}
	a.counts[kind]++
	placeholder := fmt.Sprintf("<%s-%d>", kind, a.counts[kind])
	a.ids[value] = placeholder
	return placeholder
}

func (a *probeAnonymizer) value(field string, value any) any {
	switch value := value.(type) {
	case map[string]any:
		for name, child := range value {
			value[name] = a.value(name, child)
		}
		return value
	case []any:
		for i, child := range value {
			value[i] = a.value("", child)
		}
		return value
	case string:
		if field == "encrypted_content" {
			size := 1
			for size < len(value) {
				size *= 2
			}
			return fmt.Sprintf("<encrypted up to %d bytes>", size)
		}
		if kind, ok := probeIdentifierFields[field]; ok && value != "" {
			return a.placeholder(kind, value)
		}
		return a.text(value)
	default:
		return value
	}
}

func (a *probeAnonymizer) body(data []byte) any {
	if len(data) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return a.text(string(data))
	}
	return a.value("", value)
}

func (a *probeAnonymizer) headers(source http.Header) map[string][]string {
	if source == nil {
		return nil
	}
	result := make(map[string][]string, len(source))
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		lower := strings.ToLower(canonical)
		switch {
		case canonical == "Authorization" || canonical == "Cookie" || canonical == "Set-Cookie" || canonical == "Proxy-Authorization":
			continue
		case lower == "x-opencode-session":
			result[canonical] = []string{"<session>"}
			continue
		}
		identifying := strings.Contains(lower, "request-id") || strings.Contains(lower, "trace") || strings.Contains(lower, "ray") || strings.HasSuffix(lower, "-id")
		for _, value := range values {
			if identifying {
				value = a.placeholder("header", value)
			} else {
				value = a.text(value)
			}
			result[canonical] = append(result[canonical], value)
		}
	}
	return result
}

func (a *probeAnonymizer) events(data []byte) []probeEvent {
	var events []probeEvent
	for frame := range strings.SplitSeq(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n\n") {
		var event probeEvent
		var payload []string
		for line := range strings.SplitSeq(frame, "\n") {
			if name, ok := strings.CutPrefix(line, "event:"); ok {
				event.Event = strings.TrimSpace(name)
			} else if value, ok := strings.CutPrefix(line, "data:"); ok {
				payload = append(payload, strings.TrimPrefix(value, " "))
			}
		}
		if event.Event == "" && len(payload) == 0 {
			continue
		}
		joined := strings.Join(payload, "\n")
		event.Data = a.body([]byte(joined))
		if fields, ok := event.Data.(map[string]any); ok {
			event.Type, _ = fields["type"].(string)
		}
		events = append(events, event)
	}
	return events
}

func (a *probeAnonymizer) exchange(raw rawExchange) probeExchange {
	result := probeExchange{
		Method:          raw.Method,
		Path:            raw.Path,
		RequestHeaders:  a.headers(raw.RequestHeaders),
		RequestBody:     a.body(raw.RequestBody),
		Status:          raw.Status,
		ResponseHeaders: a.headers(raw.ResponseHeaders),
		Error:           a.text(raw.Error),
	}
	if strings.HasPrefix(raw.ResponseHeaders.Get("Content-Type"), "text/event-stream") {
		result.Events = a.events(raw.ResponseBody)
	} else {
		result.Body = a.body(raw.ResponseBody)
	}
	return result
}

// findAllowance lists where any field or header named like "allowance" appears.
func findAllowance(exchanges []probeExchange) []string {
	var found []string
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch value := value.(type) {
		case map[string]any:
			for name, child := range value {
				if strings.Contains(strings.ToLower(name), "allowance") {
					shape, _ := json.Marshal(child, json.Deterministic(true))
					found = append(found, path+"."+name+" = "+string(shape))
				}
				walk(path+"."+name, child)
			}
		case []any:
			for i, child := range value {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	for i, exchange := range exchanges {
		for name, values := range exchange.ResponseHeaders {
			if strings.Contains(strings.ToLower(name), "allowance") {
				found = append(found, fmt.Sprintf("exchange %d header %s = %v", i, name, values))
			}
		}
		walk(fmt.Sprintf("exchange %d body", i), exchange.Body)
		for j, event := range exchange.Events {
			walk(fmt.Sprintf("exchange %d event %d (%s)", i, j, event.Type), event.Data)
		}
	}
	slices.Sort(found)
	return found
}

// probeModelFromEnv names the model this run probes.
func probeModelFromEnv() string {
	if value := strings.TrimSpace(os.Getenv("UNREAL_OPENCODE_GO_MODEL")); value != "" {
		return value
	}
	return probeModel
}

// probeCatalog reads the public model list with no key before any keyed
// request, and gives the model the efforts and the default effort Unreal
// itself would use.
func probeCatalog(t *testing.T, model string) modelcatalog.Model {
	t.Helper()
	listed := modelcatalog.ReadOpenCodeGo(t.Context(), &http.Client{Timeout: 20 * time.Second}, ModelsURL, []string{model})
	if !listed.Available || len(listed.Models) != 1 || listed.Models[0].ID != model {
		t.Fatalf("OpenCode Go model list has no %q: %s", model, listed.ProblemCode)
	}
	return listed.Models[0]
}
