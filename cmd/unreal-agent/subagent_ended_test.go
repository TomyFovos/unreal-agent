//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestSubagentWaitAndSendReturnImmediatelyForTerminalChildren(t *testing.T) {
	for _, status := range []operation.Status{operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: "parent", Template: "worker", Text: "task", Configuration: &subagent.Template{Workspace: t.TempDir(), Runtime: []byte(`{}`)}})
			if err != nil {
				t.Fatal(err)
			}
			op := operation.Operation{ID: "child-handle", ToolName: "SubagentStart", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: status}
			view := host.View{Generation: "generation", Operations: []operation.Operation{op}}
			socket := filepath.Join(t.TempDir(), "host.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			var inspections, controls atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/inspect" {
					controls.Add(1)
					http.Error(w, "unexpected control", 500)
					return
				}
				inspections.Add(1)
				data, _ := json.Marshal(map[string]any{"View": view})
				w.Write(append(data, '\n'))
			})}
			go server.Serve(listener)
			t.Cleanup(func() { server.Close() })
			client := gateway.NewClient(socket)
			defer client.Close()
			var out bytes.Buffer
			cli := subagentCLI{client: client, parent: "parent", name: "unreal-agent", out: &out}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := cli.wait(ctx, []string{"child-handle"}, time.Hour); err != nil {
				t.Fatal(err)
			}
			if inspections.Load() != 1 || !strings.Contains(out.String(), string(status)) {
				t.Fatal("wait polled terminal child", inspections.Load(), out.String())
			}
			out.Reset()
			if err := cli.control(ctx, "send", "child-handle", viewer.ControlRequest{Text: "continue"}); err != nil {
				t.Fatal(err)
			}
			if controls.Load() != 0 || !strings.Contains(out.String(), "has ended") || !strings.Contains(out.String(), "subagent show child-handle") {
				t.Fatal(out.String(), controls.Load())
			}
		})
	}
}
