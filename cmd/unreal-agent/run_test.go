//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeCompositionAndProviderRequest(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","role":"assistant","content":[{"type":"output_text","text":"CLI composed response"}]}]}}`+"\n\n")
	}))
	defer upstream.Close()
	dir, err := os.MkdirTemp("", "ua-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	configuration := serveConfiguration{Runtime: agentrunner.RuntimeIdentity{Version: 1, Workspace: dir, SystemPrompt: "configured", ReasoningEffort: llm.ReasoningEffort("low"), Profile: profile.Default(), Provider: provider.Selection{Version: 1, Provider: "ollama", Model: provider.Model{ID: "operator-model"}, Endpoint: upstream.URL, Auth: credential.Reference{Method: credential.None}, Source: "test", MaxAttempts: 1}}, Permissions: permission.Config{NetworkOrigins: []string{upstream.URL}}}
	data, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "host.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", configPath, "--session-directory", filepath.Join(dir, "sessions"), "--socket", socket}, io.Discard)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not drain")
		}
	}()
	client := gateway.NewClient(socket)
	defer client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = client.Methods(t.Context()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not start")
		}
		time.Sleep(time.Millisecond)
	}
	view, err := client.Open(t.Context(), host.Create, "cli-session")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal("one prompt")
	if _, err = client.Submit(t.Context(), "cli-session", view.Generation, inbox.Input{ID: "first", Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	for {
		view, err = client.Inspect(t.Context(), "cli-session", 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		complete := false
		for _, item := range view.History.Items {
			if item.Kind == sessionstore.ItemModelResponse {
				complete = true
			}
		}
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no completed response")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls %d", calls.Load())
	}
}
