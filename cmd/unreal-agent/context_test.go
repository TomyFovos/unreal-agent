//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
)

func TestContextHostGatewayAppendOnlyPrefixAndRestart(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer context-token-sensitive" {
			t.Error("external credential source changed")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, secret := range []string{"context-token-sensitive", "context-account-sensitive"} {
			if strings.Contains(string(body), secret) {
				t.Error("credential entered request context")
			}
		}
		mu.Lock()
		requests = append(requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		text := strings.Repeat("ordinary public answer ", 30)
		encoded, _ := json.Marshal(text)
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic-response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%s}]}]}}\n\n", encoded)
	}))
	defer endpoint.Close()
	workspace := privateCLIDirectory(t)
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("PIN PROJECT SNAPSHOT A: preserve exact source evidence."), 0600); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(privateCLIDirectory(t), "auth.json")
	writeCodexAuth(t, auth, "context-token-sensitive", "context-account-sensitive")
	config := codexServeConfiguration(workspace, endpoint.URL)
	config.Context = contextengine.Config{Version: 1}
	c, store, output := startInteractiveHost(t, config, "--codex-auth-file", auth)
	v, err := c.Open(t.Context(), host.Create, "context-long")
	if err != nil {
		t.Fatal(err)
	}
	const old = "KEEP-7654 in src/old_fact.go: never rewrite the orchid ledger."
	for i := 1; i <= 40; i++ {
		text := fmt.Sprintf("ordinary work number %d", i)
		if i == 5 {
			text = old
		}
		payload, _ := json.Marshal(text)
		if _, err := c.Submit(t.Context(), "context-long", v.Generation, inbox.Input{ID: inbox.ID(fmt.Sprintf("input-%d", i)), Kind: inbox.InputExternal, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		view := inspectInteractive(t, c, "context-long", func(v host.View) bool { return responseCount(v) == i || v.Failure != "" })
		if view.Failure != "" {
			t.Fatal(view.Failure)
		}
		if view.Context == nil || view.Context.EstimatedInputTokens > view.Context.Budget.Input || view.Context.Budget.Input <= 0 {
			t.Fatal("request not budgeted", view.Context)
		}
		if len(view.Operations) != 0 {
			t.Fatal("text response created tool operation")
		}
		if i == 10 {
			if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("NEW DISK SNAPSHOT B MUST NOT REPLACE A"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	submit := func(id string) {
		payload, _ := json.Marshal("What was KEEP-7654 in src/old_fact.go?")
		if _, err := c.Submit(t.Context(), "context-long", v.Generation, inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	submit("recall-1")
	view := inspectInteractive(t, c, "context-long", func(v host.View) bool { return responseCount(v) == 41 || v.Failure != "" })
	if view.Failure != "" || view.Context.Recent < 2 {
		t.Fatal("canonical history view unavailable", view.Failure, view.Context)
	}
	mu.Lock()
	last := requests[len(requests)-1]
	mu.Unlock()
	if !strings.Contains(last, old) || !strings.Contains(last, "PIN PROJECT SNAPSHOT A") || strings.Contains(last, "NEW DISK SNAPSHOT B") {
		t.Fatal("history/snapshot semantics failed")
	}
	historyPath := filepath.Join(store, "context-long.session.jsonl")
	before, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Stop(t.Context(), "context-long", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	inspectInteractive(t, c, "context-long", func(v host.View) bool { return !v.Running })
	c2, _, out2 := startInteractiveHost(t, config, "--session-directory", store, "--codex-auth-file", auth)
	v, err = c2.Open(t.Context(), host.Resume, "context-long")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := len(requests)
	mu.Unlock()
	if count != 41 {
		t.Fatal("resume sent inference without input")
	}
	c = c2
	submit("recall-after-resume")
	view = inspectInteractive(t, c, "context-long", func(v host.View) bool { return responseCount(v) == 42 || v.Failure != "" })
	if view.Failure != "" || view.Context.Cache != "append-only" || view.Context.Recent < 2 {
		t.Fatal("canonical replay broke resume", view.Failure, view.Context)
	}
	after, err := os.ReadFile(historyPath)
	if err != nil || !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("context rebuild rewrote/deleted canonical history", err)
	}
	mu.Lock()
	for i := 1; i < len(requests); i++ {
		var previous, next map[string]jsontext.Value
		if err := json.Unmarshal([]byte(requests[i-1]), &previous); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(requests[i]), &next); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(previous["tools"], next["tools"]) || !bytes.HasPrefix(next["input"], bytes.TrimSuffix(previous["input"], []byte("]"))) {
			t.Fatal("gateway request prefix changed", i)
		}
	}
	mu.Unlock()

	assertNoStoredCredentials(t, store, "context-token-sensitive", "context-account-sensitive")
	for _, o := range []*credentialOutput{output, out2} {
		assertNoCredentialLeak(t, o.String(), "context-token-sensitive", "context-account-sensitive")
	}
}
