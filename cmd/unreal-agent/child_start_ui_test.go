//go:build linux || darwin

package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Only the child command's Inspect/Extension go through the fake local gateway.
// The parent subscription stays inert, and any public submission is a failure.
type childCommandClient struct {
	*gateway.Client
	view        host.View
	submissions atomic.Int32
}

func (c *childCommandClient) Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error) {
	return host.Subscription{Initial: c.view, Events: make(chan host.Event), Cancel: func() {}}, nil
}
func (c *childCommandClient) Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error) {
	return c.view, nil
}
func (c *childCommandClient) Submit(context.Context, session.ID, string, inbox.Input) (host.Receipt, error) {
	c.submissions.Add(1)
	return host.Receipt{}, errors.New("public input must not be submitted")
}

func TestPastedMultilineChildCommandReachesProductionStartHandler(t *testing.T) {
	const input = "/children start worker openai-codex gpt-6.1-sol medium -- Reply with exactly: Codex child smoke OK.\nThen call Finish alone with status completed and that summary, with empty\nchangedFiles/tests/blockers."
	const task = "Reply with exactly: Codex child smoke OK.\nThen call Finish alone with status completed and that summary, with empty\nchangedFiles/tests/blockers."
	view := host.View{Generation: "parent-generation", Running: true}
	view.Session.Session.ID = "parent"
	view.History = host.HistoryPage{NextAfter: 1, Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "old-public-input", Kind: inbox.InputExternal, Payload: []byte(`"preserved public conversation"`)}}}}
	before, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	starts := make(chan modelRequest, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/inspect":
			body, e := json.Marshal(struct{ View host.View }{view})
			if e != nil {
				t.Error(e)
			}
			w.Write(body)
		case "/v1/extension":
			var q modelRequest
			if e := json.UnmarshalRead(r.Body, &q); e != nil {
				t.Error(e)
				return
			}
			starts <- q
			body, e := json.Marshal(modelReply{Receipt: &host.Receipt{ID: q.InputID, Sequence: 2}})
			if e != nil {
				t.Error(e)
			}
			w.Write(body)
		default:
			t.Error("unexpected gateway request", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	// A native private Unix socket, matching the real control transport.
	socket := filepath.Join(privateCLIDirectory(t), "command.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	gatewayClient := gateway.NewClient(socket)
	defer gatewayClient.Close()
	client := &childCommandClient{Client: gatewayClient, view: view}
	var delegated atomic.Int32
	command := childStartCommand(gatewayClient, "parent", func(context.Context, string) (string, bool) {
		delegated.Add(1)
		return "", false
	})
	keys := make(chan tui.Key, 128)
	out := &credentialOutput{}
	done := make(chan error, 1)
	go func() {
		done <- tui.Run(t.Context(), tui.Config{Client: client, ID: "parent", Keys: keys, Output: out, Theme: &tui.Theme{Plain: true}, Command: command})
	}()
	defer func() {
		keys <- tui.Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitInteractive(t, func() bool { return strings.Contains(out.String(), "preserved public conversation") })
	var decoder tui.Decoder
	// Fragmented bracketed-paste transport must not turn its newlines into Enter.
	for _, fragment := range []string{"\x1b[20", "0~" + input[:50], input[50:], "\x1b[201~"} {
		for _, key := range decoder.Feed([]byte(fragment)) {
			if key.Name != "" || !key.Paste {
				t.Fatal("paste generated an execution key")
			}
			keys <- key
		}
	}
	waitInteractive(t, func() bool { return strings.Contains(out.String(), "pasted:") })
	if len(starts) != 0 || client.submissions.Load() != 0 || delegated.Load() != 0 {
		t.Fatal("paste executed before Enter")
	}
	keys <- tui.Key{Name: "enter"}
	waitInteractive(t, func() bool { return len(starts) != 0 || client.submissions.Load() != 0 })
	if client.submissions.Load() != 0 {
		t.Fatal("child command became a parent/provider user message")
	}
	if len(starts) != 1 {
		t.Fatal("child handler did not submit exactly one start")
	}
	q := <-starts
	if q.Action != "child.start" || q.ID != "parent" || q.Generation != view.Generation || q.InputID == "" || q.Template != "worker" || q.Task != task {
		t.Fatal("child start lost its identity/template or multiline task")
	}
	if q.ChildRuntime == nil || q.ChildRuntime.Provider != "openai-codex" || q.ChildRuntime.Model != "gpt-6.1-sol" || q.ChildRuntime.Effort != "medium" {
		t.Fatal("child start lost explicit provider/model/effort")
	}
	waitInteractive(t, func() bool { return strings.Contains(out.String(), "Child start accepted") })
	after, err := json.Marshal(client.view)
	if err != nil || !reflect.DeepEqual(before, after) || delegated.Load() != 0 || client.submissions.Load() != 0 || len(starts) != 0 {
		t.Fatal("child command mutated parent history/Operations, repeated, or fell through")
	}
}
