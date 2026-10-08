package viewer

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestSafeToolTargetSharedDisplayWhitelist(t *testing.T) {
	for _, tt := range []struct{ name, args, want string }{
		{"read", `{"path":"harness/stream.go","env":{"PRIVATE":"private-body"},"content":"body-never-shown"}`, "harness/stream.go"},
		{"edit", `{"path":"猫.go","old_string":"private-old","new_string":"private-new"}`, "猫.go"},
		{"Bash", `{"command":"go test ./...\nprivate second command","env":{"API_KEY":"private-env"}}`, "go test ./..."},
		{"LSP", `{"action":"references","path":"parser.go","private":"private-lsp"}`, "references parser.go"},
		{"DAP", `{"command":"start","start":{"program":"app"},"env":{"KEY":"private"}}`, "start app"},
		{"unknown", `{"path":"private-unknown"}`, ""},
		{"Bash", `{"command":"curl -H 'Authorization: Bearer synthetic-private' endpoint"}`, ""},
		{"Bash", `{"command":"PRIVATE_ENV=value-never-shown go test ./..."}`, ""},
		{"Bash", `{"command":"env PRIVATE_ENV=value-never-shown go test ./..."}`, ""},
		{"read", `{"path":"api_key=synthetic-private"}`, ""},
		{"read", `not-json`, ""},
	} {
		t.Run(tt.name+tt.want, func(t *testing.T) {
			if got := SafeToolTarget(llm.ToolCall{Name: tt.name, Arguments: tt.args}); got != tt.want {
				t.Fatalf("target=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestOperationTargetsSurviveBoundedHistoryRebuildAndStatusChanges(t *testing.T) {
	call := llm.ToolCall{CallID: "call", Name: "read", Arguments: `{"path":"stream.go","private":"argument-body-never-shown"}`}
	op := operation.Operation{ID: "op", ToolName: "read", Status: operation.StatusReady, State: []byte(`{"private":"opaque-operation-never-shown"}`)}
	items := []host.HistoryItem{
		{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}},
		{Sequence: 2, RecordedAt: epoch, Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: call.CallID, Operations: []operation.Operation{op}}},
		input(3, "private-user-body-not-operation-metadata"),
	}
	m := New(Options{RecentLimit: 1})
	v := view("s", "g", 1, items...)
	v.Operations = []operation.Operation{op}
	must(t, m.Replace("s", v))
	check := func(status operation.Status, sequence sessionstore.Sequence) {
		t.Helper()
		r := row(t, m, "s")
		if len(r.Operations) != 1 || r.Operations[0].Target != "stream.go" || r.Operations[0].Status != status || r.Operations[0].Sequence != sequence {
			t.Fatal("lost safe target/status linkage", r.Operations)
		}
		b, err := json.Marshal(r.Operations, jsonv1.FormatDurationAsNano(true))
		if err != nil || strings.Contains(string(b), "never-shown") || strings.Contains(string(b), "private-user") {
			t.Fatal("raw source leaked into operation projection", err)
		}
	}
	check(operation.StatusReady, 2)
	op.Status = operation.StatusCompleted
	finish := host.HistoryItem{Sequence: 4, RecordedAt: epoch.Add(2e9), Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: call.CallID, Operations: []operation.Operation{op}}}
	must(t, m.Apply("s", host.Event{Generation: "g", Revision: 2, Kind: "item", Item: &finish}))
	check(operation.StatusCompleted, 4)
	// Resume/resync reconstructs the linkage entirely from canonical history.
	v = view("s", "new-generation", 1, append(items, finish)...)
	v.Operations = []operation.Operation{op}
	must(t, m.Replace("s", v))
	check(operation.StatusCompleted, 4)
}
