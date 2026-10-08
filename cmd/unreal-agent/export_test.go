//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
)

func TestHostGatewayCanonicalResponseExportWithoutHistoryMutation(t *testing.T) {
	body := "# Canonical response\n```go\n" + strings.Repeat("// 日本語 👩🏽‍💻 original code line\n", 240) + "```\nlast canonical line"
	encoded, _ := json.Marshal(body)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%s}]}]}}\n\n", encoded)
	}))
	defer endpoint.Close()
	workspace := privateCLIDirectory(t)
	auth := filepath.Join(privateCLIDirectory(t), "auth.json")
	writeCodexAuth(t, auth, "export-fake-token", "export-fake-account")
	client, store, _ := startInteractiveHost(t, codexServeConfiguration(workspace, endpoint.URL), "--codex-auth-file", auth)
	v, err := client.Open(t.Context(), host.Create, "export-canonical")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal("synthetic local response fixture")
	if _, err = client.Submit(t.Context(), "export-canonical", v.Generation, inbox.Input{ID: "export-input", Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	v = inspectInteractive(t, client, "export-canonical", func(v host.View) bool { return responseCount(v) == 1 || v.Failure != "" })
	if v.Failure != "" {
		t.Fatal(v.Failure)
	}
	path := filepath.Join(store, "export-canonical.session.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	exports := filepath.Join(privateCLIDirectory(t), "exports")
	choices, err := tui.ResponseChoices(t.Context(), client, "export-canonical", v.History.NextAfter)
	if err != nil || len(choices) != 1 || choices[0].TurnNumber != 1 {
		t.Fatal("incorrect canonical response choices", err)
	}
	last, err := tui.ExportLastResponse(t.Context(), client, "export-canonical", v.History.NextAfter, exports)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := tui.ExportResponse(t.Context(), client, "export-canonical", choices[0], exports)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{last, selected} {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != body {
			t.Fatal("gateway canonical export changed original", err)
		}
	}
	all, err := tui.ExportConversation(t.Context(), client, "export-canonical", v.History.NextAfter, exports)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(all)
	if err != nil || !strings.Contains(string(got), body) || !strings.Contains(string(got), "synthetic local response fixture") {
		t.Fatal("full public conversation missing", err)
	}
	for _, protected := range []string{"export-fake-token", "export-fake-account", "authorization", "project_instructions"} {
		if strings.Contains(string(got), protected) {
			t.Fatal("non-public metadata exported")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("export modified Session history", err)
	}
	post, err := client.Inspect(t.Context(), "export-canonical", 0, 128)
	if err != nil || len(post.Operations) != 0 {
		t.Fatal("export/text response created an operation")
	}
}
