package gateway

import (
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

func TestProjectInstructionFailuresKeepTheirCode(t *testing.T) {
	// A missing workspace wraps fs.ErrNotExist but is not a missing session.
	err := fmt.Errorf("open: %w", &projectinstructions.Error{Code: projectinstructions.CodeInvalidWorkspace, Path: "/w", Err: fs.ErrNotExist})
	if got := errorCode(err); got != "project_instructions_invalid_workspace" {
		t.Fatalf("errorCode = %q", got)
	}
	if got := errorCode(&projectinstructions.Error{Code: projectinstructions.CodeSymlink}); got != "project_instructions_symlink" {
		t.Fatalf("errorCode = %q", got)
	}
}

func TestGatewayProjectInstructionsPublicProjection(t *testing.T) {
	const marker = "unique-AGENTS-secret-marker-27-gateway"
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	owner, client, _, _ := fixture(t)
	current, err := owner.Create(t.Context(), host.Options{ID: "projection", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := projectinstructions.FromContent([]byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	check := func(raw []byte, v *host.View) {
		t.Helper()
		if strings.Contains(string(raw), marker) {
			t.Fatal("gateway exposed project instruction content")
		}
		if v != nil && (v.ProjectInstructions == nil || *v.ProjectInstructions != expected.Metadata()) {
			t.Fatalf("gateway metadata lost: %+v", v.ProjectInstructions)
		}
	}
	response, _, err := client.connect(bounded(t), "inspect", request{ID: current.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	check(raw, f.View)
	if f.View == nil || f.View.History.NextAfter != 1 || !f.View.History.More {
		t.Fatal("gateway history pagination changed")
	}
	stream, scanner, err := client.connect(bounded(t), "subscribe", request{ID: current.ID, Limit: 128, Capacity: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	first, err := next(scanner)
	if err != nil {
		t.Fatal(err)
	}
	check(scanner.Bytes(), first.View)
	if _, err = client.Submit(bounded(t), current.ID, current.Generation, prompt("projection-input", "hello")); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := next(scanner)
		if err != nil {
			t.Fatal(err)
		}
		check(scanner.Bytes(), nil)
		if event.Event != nil && event.Event.Item != nil && event.Event.Item.Sequence > first.View.History.NextAfter {
			break
		}
	}
}
