package modelcatalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// The public list as read on 2026-11-03 (28 models).
func openCodeGoList(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/opencode-go-models-2026-11-03.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseOpenCodeGoKeepsOnlyOfferedModels(t *testing.T) {
	c := ParseOpenCodeGo(openCodeGoList(t), []string{"deepseek-v4.1-flash"})
	if !c.Available || c.Source != "OpenCode Go model list" || len(c.Models) != 1 {
		t.Fatal("catalog", c.Available, c.Source, len(c.Models), c.ProblemCode)
	}
	m := c.Models[0]
	if m.ID != "deepseek-v4.1-flash" || m.Name != "DeepSeek V4.1 Flash" || m.ContextWindow != 1000000 {
		t.Fatal("model", m.ID, m.Name, m.ContextWindow)
	}
	if !slices.Equal(m.Efforts, []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortHigh, llm.ReasoningEffortMax}) || m.DefaultEffort != llm.ReasoningEffortHigh {
		t.Fatal("efforts", m.Efforts, m.DefaultEffort)
	}
	if _, err := Configured(c.Models); err != nil {
		t.Fatal("catalog fails the shared model validation", err)
	}
	// Offered order decides, and a listed model is never added on its own.
	c = ParseOpenCodeGo(openCodeGoList(t), []string{"glm-5.3", "deepseek-v4.1-flash", "not-listed"})
	if len(c.Models) != 2 || c.Models[0].ID != "glm-5.3" || c.Models[1].ID != "deepseek-v4.1-flash" {
		t.Fatal("offered order", c.Models)
	}
}

func TestParseOpenCodeGoFailures(t *testing.T) {
	flash := `{"id":"deepseek-v4.1-flash","name":"Flash","capabilities":{"reasoning":{"supported":true,"effort":["none","low","high","max","ultra"]}},"limit":{"context":1000}}`
	for _, tc := range []struct {
		name, body, code string
	}{
		{"not_json", `{`, "opencode_go_list_invalid"},
		{"missing", `{"data":[{"id":"other"}]}`, "opencode_go_model_missing"},
		{"offered_entry_broken", `{"data":[{"id":"deepseek-v4.1-flash","capabilities":"text"}]}`, "opencode_go_list_invalid"},
	} {
		if c := ParseOpenCodeGo([]byte(tc.body), []string{"deepseek-v4.1-flash"}); c.Available || c.ProblemCode != tc.code || c.Problem != "catalog unavailable" {
			t.Fatal(tc.name, c.Available, c.ProblemCode)
		}
	}
	// Another model's unexpected format does not hide the offered one, and only
	// Unreal's own efforts are kept.
	c := ParseOpenCodeGo([]byte(`{"data":[{"id":"other","capabilities":"text"},`+flash+`]}`), []string{"deepseek-v4.1-flash"})
	if !c.Available || !slices.Equal(c.Models[0].Efforts, []llm.ReasoningEffort{"low", "high", "max"}) {
		t.Fatal("other entry broke the list", c.ProblemCode, c.Models)
	}
}

func TestParseOpenCodeGoEffortsAndDisplayText(t *testing.T) {
	parse := func(entry string) Model {
		t.Helper()
		c := ParseOpenCodeGo([]byte(`{"data":[`+entry+`]}`), []string{"m"})
		if !c.Available {
			t.Fatal(entry, c.ProblemCode)
		}
		return c.Models[0]
	}
	if m := parse(`{"id":"m","name":"Bad\u001b[2J","description":"line\nbreak","capabilities":{"reasoning":{"supported":false,"effort":["low"]}}}`); m.Name != "m" || m.Description != "" || m.SupportsEffort == nil || *m.SupportsEffort || m.Efforts != nil {
		t.Fatal("control characters or unsupported reasoning", m)
	}
	if m := parse(`{"id":"m","capabilities":{"reasoning":{"supported":true}}}`); !m.EffortUnknown || m.DefaultEffort != "" {
		t.Fatal("reasoning without listed efforts", m)
	}
	if m := parse(`{"id":"m","capabilities":{"reasoning":{"supported":true,"effort":["medium","xhigh"]}}}`); m.DefaultEffort != llm.ReasoningEffortMedium {
		t.Fatal("default without high", m.DefaultEffort)
	}
}

func TestOpenCodeGoNameAndHints(t *testing.T) {
	if ProviderName("opencode-go") != "OpenCode Go" {
		t.Fatal(ProviderName("opencode-go"))
	}
	for reason, want := range map[AvailabilityReason]string{AuthRequired: "/login の手順で API キーを保存", CatalogUnavailable: "/model refresh", ProviderCheckFailed: "API キーと通信"} {
		hint := reason.ProviderHint("opencode-go")
		if !strings.Contains(hint, want) || strings.Contains(hint, "codex") || strings.Contains(hint, "CLI") {
			t.Fatal(reason, hint)
		}
	}
	if AuthRequired.ProviderHint("openai-codex") != "codex login でログインしてください" {
		t.Fatal("Codex hint changed")
	}
}

func TestReadOpenCodeGoSendsNoKeyAndFollowsNoRedirect(t *testing.T) {
	list := openCodeGoList(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inference/go/v1/models":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") != "unreal-agent" {
				t.Error("request", r.Method, r.Header)
			}
			w.Write(list)
		case "/moved":
			http.Redirect(w, r, "/inference/go/v1/models", http.StatusFound)
		case "/large":
			w.Write([]byte(`{"data":[` + strings.Repeat(" ", maxOpenCodeGoList) + `]}`))
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	offered := []string{"deepseek-v4.1-flash"}
	if c := ReadOpenCodeGo(t.Context(), server.Client(), server.URL+"/inference/go/v1/models", offered); !c.Available || c.FetchedAt.IsZero() || c.Models[0].ID != "deepseek-v4.1-flash" {
		t.Fatal("read", c.ProblemCode)
	}
	for path, code := range map[string]string{"/moved": "opencode_go_list_status", "/down": "opencode_go_list_status", "/large": "opencode_go_list_unreadable"} {
		if c := ReadOpenCodeGo(t.Context(), server.Client(), server.URL+path, offered); c.Available || c.ProblemCode != code {
			t.Fatal(path, c.ProblemCode)
		}
	}
	if c := ReadOpenCodeGo(t.Context(), server.Client(), "http://127.0.0.1:1/inference/go/v1/models", offered); c.Available || c.ProblemCode != "opencode_go_list_unreachable" {
		t.Fatal("unreachable", c.ProblemCode)
	}
}

// The public list as read on 2026-11-05 (29 models, 24 offered).
func openCodeGoList20261105(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/opencode-go-models-2026-11-05.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Flash, then the 23 models that each passed every scenario of the live
// surveys (docs/opencode-go.md; 2026-11-05, and Gemini 4 Argon 2026-11-14),
// in the public list's order.
var openCodeGoOffered20261105 = []string{
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"glm-5.2",
	"glm-5.3",
	"grok-4.7",
	"glm-5.3-flash",
	"gpt-5.6-luna",
	"gpt-6-luna",
	"claude-haiku-5-5",
	"hy3",
	"hy4-preview",
	"kimi-k2.7-code",
	"kimi-k3",
	"mimo-v2.6-pro",
	"minimax-m3",
	"minimax-m3.1-flash",
	"longcat-2.0",
	"step-5-preview",
	"qwen3.8-max",
	"qwen3.8-flash",
	"qwen3.7-plus",
	"kimi-k3.1-pro",
	"gemini-4-argon",
	"hy4",
}

func TestParseOpenCodeGoKeepsTheSurveyedModels(t *testing.T) {
	c := ParseOpenCodeGo(openCodeGoList20261105(t), openCodeGoOffered20261105)
	if !c.Available || c.Source != "OpenCode Go model list" {
		t.Fatal("catalog", c.Available, c.Source, c.ProblemCode)
	}
	ids := make([]string, len(c.Models))
	for i, m := range c.Models {
		ids[i] = m.ID
	}
	if len(c.Models) != 24 || !slices.Equal(ids, openCodeGoOffered20261105) {
		t.Fatal("models", len(c.Models), ids)
	}
	byID := map[string]Model{}
	for _, m := range c.Models {
		byID[m.ID] = m
	}
	if m := byID["deepseek-v4.1-flash"]; !slices.Equal(m.Efforts, []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortHigh, llm.ReasoningEffortMax}) || m.DefaultEffort != llm.ReasoningEffortHigh || m.ContextWindow != 1000000 {
		t.Fatal("deepseek-v4.1-flash", m.Efforts, m.DefaultEffort, m.ContextWindow)
	}
	if m := byID["kimi-k3"]; !slices.Equal(m.Efforts, []llm.ReasoningEffort{llm.ReasoningEffortMax}) || m.DefaultEffort != llm.ReasoningEffortMax {
		t.Fatal("kimi-k3", m.Efforts, m.DefaultEffort)
	}
	if m := byID["qwen3.8-max"]; !slices.Equal(m.Efforts, []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortXHigh}) || m.DefaultEffort != llm.ReasoningEffortLow {
		t.Fatal("qwen3.8-max", m.Efforts, m.DefaultEffort)
	}
	// "none" is not one of Unreal's efforts, so it is dropped.
	if m := byID["gpt-6-luna"]; !slices.Equal(m.Efforts, []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh, llm.ReasoningEffortXHigh, llm.ReasoningEffortMax}) || m.DefaultEffort != llm.ReasoningEffortHigh {
		t.Fatal("gpt-6-luna", m.Efforts, m.DefaultEffort)
	}
	for _, id := range []string{"kimi-k2.7-code", "minimax-m3"} {
		if m := byID[id]; len(m.Efforts) != 0 || m.DefaultEffort != "" || !m.EffortUnknown {
			t.Fatal(id, m.Efforts, m.DefaultEffort, m.EffortUnknown)
		}
	}
}

func TestParseOpenCodeGoLeavesOutTheExcludedModels(t *testing.T) {
	excluded := []string{"mimo-v2.6-flash", "muse-spark-1.2-contributor", "muse-spark-1.3-contributor", "beam-free", "iceman-contributor-free"}
	raw := string(openCodeGoList20261105(t))
	for _, id := range excluded {
		if !strings.Contains(raw, `"`+id+`"`) {
			t.Fatal("the raw list no longer lists", id)
		}
	}
	for _, m := range ParseOpenCodeGo(openCodeGoList20261105(t), openCodeGoOffered20261105).Models {
		if slices.Contains(excluded, m.ID) {
			t.Fatal("excluded model was offered", m.ID)
		}
	}
}
