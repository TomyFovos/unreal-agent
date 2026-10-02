package subagent

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestReadChildProjectsInstructionsAndInheritedConfiguration(t *testing.T) {
	const marker = "unique-AGENTS-secret-marker-child-projection"
	snapshot, err := projectinstructions.FromContent([]byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	id := operation.ID("operation")
	config := ChildConfig{Version: 1, ParentID: "parent", ChildID: ChildID("parent", id), OperationID: id,
		SessionDirectory: dir, Workspace: t.TempDir(), Runtime: []byte("{}"), ReadyID: "ready:operation",
		Task: "delegated", Policy: permission.Config{Tools: []string{"Finish"}}, ProjectInstructions: &snapshot}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateWithHostRecords(t.Context(), config.ChildID,
		sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostProjectInstructions, ProjectInstructions: &snapshot},
		sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: data})
	if err != nil {
		t.Fatal(err)
	}
	for cursor := sessionstore.BeforeFirst; ; {
		child, err := ReadChild(t.Context(), dir, config.ChildID, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(child.View)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), marker) {
			t.Fatal("public child view exposed inherited instructions")
		}
		if child.View.ProjectInstructions == nil || *child.View.ProjectInstructions != snapshot.Metadata() {
			t.Fatal("child metadata lost")
		}
		if child.Configuration == nil || child.Configuration.ProjectInstructions == nil || *child.Configuration.ProjectInstructions != snapshot {
			t.Fatal("internal replay configuration lost instructions")
		}
		if len(child.View.History.Items) != 1 || child.View.History.Items[0].Sequence != cursor+1 || child.View.History.NextAfter != cursor+1 {
			t.Fatal("child projection changed history cursor")
		}
		if !child.View.History.More {
			break
		}
		cursor = child.View.History.NextAfter
	}
	canonical, err := store.Items(t.Context(), config.ChildID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if *canonical.Items[0].Data.(sessionstore.HostRecord).ProjectInstructions != snapshot {
		t.Fatal("canonical child snapshot changed")
	}
	if string(canonical.Items[1].Data.(sessionstore.HostRecord).Configuration) != string(data) {
		t.Fatal("canonical child configuration changed")
	}
}
