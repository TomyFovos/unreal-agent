//go:build linux || darwin

package main

import (
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestModelSelectionGatewayExternalCredentialsAndResume(t *testing.T) {
	type wireRequest struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	requests := make(chan wireRequest, 4)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer model-token-sensitive" || r.Header.Get("ChatGPT-Account-ID") != "model-account-sensitive" {
			t.Error("model switch changed credential source")
		}
		var req wireRequest
		b, e := io.ReadAll(r.Body)
		if e != nil || json.Unmarshal(b, &req) != nil {
			t.Error("invalid request")
		}
		requests <- req
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n")
	}))
	defer endpoint.Close()
	dir := privateCLIDirectory(t)
	auth := filepath.Join(dir, "auth.json")
	writeCodexAuth(t, auth, "model-token-sensitive", "model-account-sensitive")
	cache := filepath.Join(dir, "models_cache.json")
	// This fixture tests selection/resume, so leave room for the real tool
	// schemas, required instructions and the compaction response reserve.
	cacheData := []byte(`{"identity":{"account_id":"cache-account-sensitive"},"models":[{"slug":"model-b","display_name":"Model B","visibility":"list","supported_reasoning_levels":[{"effort":"high"}],"default_reasoning_level":"high","context_window":32768}]}`)
	if err := os.WriteFile(cache, cacheData, 0600); err != nil {
		t.Fatal(err)
	}
	c, store, out := startInteractiveHost(t, codexServeConfiguration(dir, endpoint.URL), "--codex-auth-file", auth)
	v, err := c.Open(t.Context(), host.Create, "model-test")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "model-test"})
	if err != nil || catalog.Catalog == nil || !catalog.Catalog.Available || len(catalog.Catalog.Models) != 1 {
		t.Fatal(catalog, err)
	}
	choice := sessionstore.RuntimeSelection{Version: 1, RequestID: "choice-1", Model: "model-b", Effort: llm.ReasoningEffortHigh}
	invalid := choice
	invalid.RequestID, invalid.Effort = "invalid-effort", llm.ReasoningEffortLow
	if _, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "model-test", Generation: v.Generation, Selection: invalid}); err == nil {
		t.Fatal("accepted effort absent from selected model catalog")
	}
	if err = os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	unavailable, err := modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.catalog", ID: "model-test"})
	if err != nil || unavailable.Catalog == nil || unavailable.Catalog.Available || len(unavailable.Catalog.Models) != 0 {
		t.Fatal("invented catalog when missing", err)
	}
	if _, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "model-test", Generation: v.Generation, Selection: choice}); err == nil {
		t.Fatal("accepted stale picker when catalog became unavailable")
	}
	before, err := c.Inspect(t.Context(), "model-test", 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range before.History.Items {
		if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Selection != nil {
			t.Fatal("failed confirmation altered canonical runtime")
		}
	}
	if err = os.WriteFile(cache, cacheData, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = modelExchange(t.Context(), c.Extension, modelRequest{Action: "model.select", ID: "model-test", Generation: v.Generation, Selection: choice})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatal("selection triggered speculative inference")
	}
	for n := range 2 {
		if n == 1 {
			v, err = c.Open(t.Context(), host.Resume, "model-test")
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err = c.Submit(t.Context(), "model-test", v.Generation, inbox.Input{ID: inbox.ID("input-" + string(rune('a'+n))), Kind: inbox.InputExternal, Payload: []byte(`"prompt-sensitive"`)})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case req := <-requests:
			if req.Model != "model-b" || req.Reasoning.Effort != "high" {
				t.Fatal(req)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no fake request")
		}
		inspectInteractive(t, c, "model-test", func(v host.View) bool { return responseCount(v) == n+1 })
		if _, err = c.Stop(t.Context(), "model-test", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
			t.Fatal(err)
		}
		inspectInteractive(t, c, "model-test", func(v host.View) bool { return !v.Running })
	}
	// A new Host and factory must restore the applied choice from the same
	// canonical store while still validating the original startup identity.
	c2, _, out2 := startInteractiveHost(t, codexServeConfiguration(dir, endpoint.URL), "--session-directory", store, "--codex-auth-file", auth)
	v, err = c2.Open(t.Context(), host.Resume, "model-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c2.Submit(t.Context(), "model-test", v.Generation, inbox.Input{ID: "input-restart", Kind: inbox.InputExternal, Payload: []byte(`"prompt-sensitive"`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-requests:
		if req.Model != "model-b" || req.Reasoning.Effort != "high" {
			t.Fatal("factory restart lost selection", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not send fake request")
	}
	inspectInteractive(t, c2, "model-test", func(v host.View) bool { return responseCount(v) == 3 })
	if _, err = c2.Stop(t.Context(), "model-test", v.Generation, inbox.StopWhenIdle, "test"); err != nil {
		t.Fatal(err)
	}
	inspectInteractive(t, c2, "model-test", func(v host.View) bool { return !v.Running })
	assertNoStoredCredentials(t, store, "model-token-sensitive", "model-account-sensitive", "cache-account-sensitive")
	assertNoCredentialLeak(t, out.String(), "model-token-sensitive", "model-account-sensitive")
	assertNoCredentialLeak(t, out2.String(), "model-token-sensitive", "model-account-sensitive")
	encoded, _ := json.Marshal(catalog)
	if strings.Contains(string(encoded), "cache-account-sensitive") {
		t.Fatal("catalog leaked account identity")
	}
}
