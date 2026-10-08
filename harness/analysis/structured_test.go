package analysis

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestStructuredBridgeModeIsSafeMetadataOnly(t *testing.T) {
	a := New("structured")
	a.Apply(host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"opus"}},"ClaudeCode":{"managedPolicyMode":"trust","ToolBridge":{"Enabled":true,"Mode":"structured"},"credential":"credential-sensitive","structured_output":"protocol-sensitive"}}`)}})
	r := a.Snapshot(time.Now(), true, false, nil, nil)
	if r.ToolBridgeMode != "structured" || r.ToolCapability != "Unreal tools via structured actions" || !strings.Contains(strings.Join(Lines(r, "Overview", false), "\n"), "StructuredOutput serializer only") {
		t.Fatal("missing structured boundary metadata")
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "credential-sensitive") || strings.Contains(string(data), "protocol-sensitive") {
		t.Fatal("private values leaked into analysis/export")
	}
	a.Apply(host.HistoryItem{Sequence: 2, Kind: sessionstore.ItemFork, Data: sessionstore.Fork{ParentID: "parent", PreviousTurnID: "previous"}})
	r = a.Snapshot(time.Now(), true, false, nil, nil)
	if r.ToolBridgeEnabled || r.ToolBridgeMode != "" {
		t.Fatal("fork guessed an inherited bridge capability")
	}
}
