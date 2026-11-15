package agentrunner

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestRunnerThinkingNoneOmitsReasoning(t *testing.T) {
	for _, selected := range DefaultProviders() {
		for _, composition := range []string{"conservative", "development"} {
			t.Run(selected.Name+"/"+composition, func(t *testing.T) {
				var calls int
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var body map[string]jsontext.Value
					if err := json.UnmarshalRead(r.Body, &body); err != nil {
						t.Error(err)
					}
					for _, key := range []string{"reasoning", "reasoning_effort", "thinking_level"} {
						if _, ok := body[key]; ok {
							t.Error("disabled reasoning sent", key)
						}
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
				}))
				defer endpoint.Close()
				environment := map[string]string{llmProviderEnvironment: selected.Name, llmBaseURLEnvironment: endpoint.URL, llmAPIKeyEnvironment: "synthetic-key", "OPENAI_CODEX_ACCESS_TOKEN": "synthetic-token", "OPENAI_CODEX_ACCOUNT_ID": "synthetic-account"}
				var output, diagnostics bytes.Buffer
				code := RunMain(t.Context(), []string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()}, func(name string) string { return environment[name] }, func() []string { return nil }, strings.NewReader(fmt.Sprintf(`{"prompt":"hello","model":"fake-model","profile":%q,"thinking_level":"none"}`, composition)), &output, &diagnostics, Config{Name: "runner", Providers: DefaultProviders(), ParseRequest: parseTestRequest})
				if code != 0 || calls != 1 {
					t.Fatalf("code=%d calls=%d diagnostics=%s", code, calls, diagnostics.String())
				}
				for _, item := range decodeHostLogItems(t, output.Bytes()) {
					if item.Kind == sessionstore.ItemInput {
						in := item.Data.(inbox.Input)
						if control, err := in.DecodeControlMessage(); err == nil && control.Mode == inbox.UpdateSettings {
							t.Fatal("empty settings message was admitted")
						}
					}
				}
			})
		}
	}
}
