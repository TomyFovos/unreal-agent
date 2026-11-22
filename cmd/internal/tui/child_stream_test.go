package tui

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

const workerStreamHelper = "UNREAL_TUI_WORKER_STREAM_HELPER"

func workerStreamCredentials() credential.Resolver {
	return credential.ResolverFunc(func(context.Context, credential.Reference) (credential.Material, error) {
		return credential.Material{Token: credential.NewSecret("test-token"), AccountID: "test-account"}, nil
	})
}

func TestWorkerStreamChildProcess(t *testing.T) {
	if os.Getenv(workerStreamHelper) != "1" {
		return
	}
	if err := subagent.Serve(t.Context(), os.Stdin, os.Stdout, agentrunner.NewChildFactory(agentrunner.RuntimeConfig{Credentials: workerStreamCredentials()}, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestOrchestrationDeepSeekWorkerStream(t *testing.T) {
	if !buildfeatures.OpenCodeGoEnabled {
		t.Skip("OpenCode Go is disabled in this build")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	const leadModel, workerModel = "gpt-6.2-sol", "deepseek-v4.1-flash"
	const briefTitle = "Inspect the streamed worker"
	const briefLookup = `{"text":"brief title"}`
	const openCodePath = "/inference/go/openai/v1"
	const leadUsage = `{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":900}}`
	const workerUsage = `{"input_tokens":83417,"output_tokens":430,"input_tokens_details":{"cached_tokens":82816},"output_tokens_details":{"reasoning_tokens":294},"opencode":{"billing_source":"go"}}`
	delta, complete := make(chan struct{}), make(chan struct{})
	var workerRequests, leadRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(id, name, args, usage string) {
			field := ""
			if usage != "" {
				field = `,"usage":` + usage
			}
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":%q,\"call_id\":%q,\"name\":%q,\"arguments\":%q,\"status\":\"completed\"}]%s}}\n\n", id, id, id, name, args, field)
		}
		switch r.URL.Path {
		case "/responses":
			w.Header().Set("X-Codex-Primary-Used-Percent", "10")
			if leadRequests.Add(1) == 1 {
				send("lead-start", "SubagentStart", `{"template":"worker","task":"Long instructions","brief":{"title":"`+briefTitle+`"}}`, leadUsage)
				return
			}
			<-r.Context().Done()
		case openCodePath + "/responses":
			var body struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
				Input  []struct {
					Type   string         `json:"type"`
					CallID string         `json:"call_id"`
					Output jsontext.Value `json:"output"`
				} `json:"input"`
			}
			if err := json.UnmarshalRead(r.Body, &body); err != nil || body.Model != workerModel || !body.Stream {
				t.Errorf("invalid OpenCode Go stream request: %v", err)
			}
			switch workerRequests.Add(1) {
			case 1:
				send("worker-with-usage", "SendParent", briefLookup, workerUsage)
			case 2:
				var answered bool
				for _, item := range body.Input {
					if item.Type == "function_call_output" && item.CallID == "worker-with-usage" && bytes.Contains(item.Output, []byte(`"text":"`+briefTitle+`"`)) {
						answered = true
					}
				}
				if !answered {
					t.Error("worker continuation lost the routed brief title")
				}
				select {
				case <-delta:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ephemeral worker draft\"}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-complete:
				case <-r.Context().Done():
					return
				}
				send("worker-without-usage", "SendParent", briefLookup, "")
			case 3:
				<-r.Context().Done()
			default:
				t.Error("unexpected extra OpenCode Go request")
			}
		default:
			t.Errorf("unexpected Responses path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	dir, workspace := t.TempDir(), t.TempDir()
	childTemplate := func(layer, id, model, endpoint string, method credential.Method, policy permission.Config) subagent.Template {
		t.Helper()
		modelSpec := provider.Model{ID: model}
		registry, err := provider.New(provider.Defaults(map[string][]provider.Model{id: {modelSpec}})...)
		if err != nil {
			t.Fatal(err)
		}
		identity := agentrunner.RuntimeIdentity{Version: 1, Workspace: workspace, Profile: profile.Default(), Provider: provider.Selection{
			Version: 1, Provider: id, Model: modelSpec, Endpoint: endpoint, Auth: credential.Reference{Provider: id, ID: "test", Method: method}, MaxAttempts: 1, Source: "test",
		}}
		_, raw, err := agentrunner.NewRuntimeFactory(agentrunner.RuntimeConfig{Identity: identity, SessionDirectory: dir, Providers: registry, Credentials: workerStreamCredentials()})
		if err != nil {
			t.Fatal(err)
		}
		return subagent.Template{Layer: layer, Workspace: workspace, Runtime: raw, Policy: policy}
	}
	worker := childTemplate("worker", "opencode-go", workerModel, server.URL+openCodePath, credential.APIKey, permission.Config{
		Tools: []string{"SendParent", "Finish"}, NetworkOrigins: []string{server.URL},
	})
	lead := childTemplate("lead", "openai-codex", leadModel, server.URL, credential.OAuth, permission.Config{
		Tools: []string{"SubagentStart", "SendParent", "Finish"}, ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true,
	})
	lead.Subagents = map[string]subagent.Template{"worker": worker}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(ctx, host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		owner, _ := host.SessionFromContext(ctx)
		manager, err := subagent.NewManager(ctx, subagent.Config{Owner: owner, Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestWorkerStreamChildProcess$"},
			Environment: append(os.Environ(), workerStreamHelper+"=1"), Templates: map[string]subagent.Template{"lead": lead}})
		if err != nil {
			return host.Runtime{}, err
		}
		return host.Runtime{Builder: contextbuilder.NewBuilder(), Tools: tool.NewRegistry(tool.StaticTranslators{}), LLM: archiveAnalysisModel{},
			Operations: operation.NewLocalOperationManager(ctx, manager), Close: manager.Close}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	parent, err := h.Create(ctx, host.Options{ID: "main", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := subagent.NewSpec(subagent.Plan{Version: 1, Action: "start", ParentID: parent.ID, Template: "lead", Configuration: &lead, Text: "Delegate the test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.SubmitOperation(ctx, parent.Generation, "start", "start-lead", "SubagentStart", spec); err != nil {
		t.Fatal(err)
	}

	reader := viewer.ParentReader{Host: h, ParentID: parent.ID, Directory: dir}
	handler := viewer.NewExtensionHandler(h, dir)
	remote := viewer.Remote{ParentID: parent.ID, Parent: reader, Call: func(ctx context.Context, raw jsontext.Value) (jsontext.Value, error) {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/extension", bytes.NewReader(raw))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return jsontext.Value(w.Body.Bytes()), nil
	}}
	panel := viewer.NewPanel(ctx, parent.ID, viewer.NewClient(remote, viewer.New(viewer.SubagentOptions()), nil))
	defer panel.Close()
	panel.SetTopologyVisible(true)

	s := Snapshot{ID: parent.ID, Connected: true, Running: true}
	u := UIState{View: ViewOrchestration, Theme: Theme{Plain: true}, cache: &bodyCache{}}
	waitWorker := func(phase host.ModelPhase, responses int) viewer.Row {
		t.Helper()
		for {
			u.Now = time.Now()
			u.Viewer = panel.Snapshot(u.Now)
			for _, row := range u.Viewer.Rows {
				if row.Depth == 2 && row.Label == briefTitle && row.ModelPhase == phase && row.Usage.Responses == responses {
					return row
				}
			}
			select {
			case <-panel.Updates():
			case <-ctx.Done():
				t.Fatalf("worker did not reach %s with %d responses: %+v", phase, responses, u.Viewer.Rows)
			}
		}
	}
	waitWorker(host.ModelRequesting, 1)
	if got := frameText(RenderFrame(s, u, 150, 28)); !strings.Contains(got, u.Theme.spinner(u.Now.UnixMilli())+" 考え中") {
		t.Fatal("worker did not show request start", got)
	}
	close(delta)
	row := waitWorker(host.ModelStreaming, 1)
	if row.Selection == nil || row.Selection.Provider != "opencode-go" || row.Selection.Model != workerModel || row.Runtime != viewer.RuntimeUnknown {
		t.Fatalf("unexpected worker identity: %+v", row)
	}
	if _, err := remote.Inspect(ctx, row.ID, sessionstore.BeforeFirst, 1); !errors.Is(err, viewer.ErrUnavailable) {
		t.Fatalf("ordinary direct-child read of a grandchild: %v", err)
	}
	v, err := remote.InspectTopology(ctx, row.ID, sessionstore.BeforeFirst, 256)
	if err != nil {
		t.Fatal(err)
	}
	var recordedTPS float64
	for _, item := range v.History.Items {
		if r, ok := item.Data.(sessionstore.ModelResponse); ok && r.Response.Generation != nil &&
			r.Response.Usage.AuthMethod == "api_key" && bytes.Contains(r.Response.Usage.Raw, []byte(`"billing_source":"go"`)) {
			recordedTPS, _ = r.Response.Generation.OutputTPS()
		}
	}
	if recordedTPS <= 0 || row.OutputTPS != recordedTPS || row.LastUsage.Input != 83417 || row.LastUsage.CachedInput != 82816 {
		t.Fatalf("worker projection lost response counters or timing: recorded speed %f, row %+v", recordedTPS, row)
	}
	frame := strings.Split(frameText(RenderFrame(s, u, 150, 28)), "\n")
	var workerLine, leadLine string
	for i, line := range frame[:len(frame)-1] {
		switch {
		case strings.Contains(line, briefTitle) && strings.Contains(line, workerModel):
			workerLine = frame[i+1]
		case strings.Contains(line, leadModel):
			leadLine = frame[i+1]
		}
	}
	for _, want := range []string{u.Theme.spinner(u.Now.UnixMilli()) + " 生成中", fmt.Sprintf("%.0f t/s · 99.3%%", recordedTPS)} {
		if !strings.Contains(workerLine, want) {
			t.Fatalf("worker card missing %q: %s", want, workerLine)
		}
	}
	if strings.Contains(workerLine, "ephemeral worker draft") || !strings.Contains(leadLine, "t/s · 90.0%") {
		t.Fatal("unexpected draft or Codex lead statistics", workerLine, leadLine)
	}
	close(complete)
	row = waitWorker(host.ModelRequesting, 2)
	if row.LastUsage.Known || row.OutputTPSKnown || cardStats(row) != "" {
		t.Fatalf("missing usage displayed as measured: %+v, %q", row, cardStats(row))
	}
}
