//go:build linux || darwin

package native

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
)

func TestWriteAbsentRevisionInsideWorkspaceAndOutsideDiagnostic(t *testing.T) {
	e, _ := executor(t)
	absent := mutation.Revision{}
	outside := filepath.Join(t.TempDir(), "sample.txt")
	request := Request{Version: Version, Action: "write", Path: outside, Expected: &absent, Content: "before\n"}
	r := e.Execute(t.Context(), "outside", request)
	if r.Code != "invalid" || r.Mutation == nil || r.Mutation.Code != mutation.Invalid || r.Denial != nil {
		t.Fatal("workspace invalid misclassified as Permission", r)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("outside effect", err)
	}
	encoded := EncodeResult(r)
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["reason"] != "workspace_boundary" || fields["mutation"].(map[string]any)["reason"] != "workspace_boundary" {
		t.Fatalf("generic invalid hides exact boundary: %s", encoded)
	}
	request.Path = "sample.txt"
	if r = e.Execute(t.Context(), "create", request); r.Code != "applied" {
		t.Fatal("exists:false must create in workspace", r)
	}
	read := e.Execute(t.Context(), "read", Request{Version: Version, Action: "read", Path: request.Path})
	if read.Code != "ok" || read.Content != request.Content || read.Revision == nil || !read.Revision.Exists {
		t.Fatal(read)
	}
	edit := Request{Version: Version, Action: "edit", Path: request.Path, Expected: read.Revision, OldText: "before", NewText: "after"}
	if r = e.Execute(t.Context(), "edit", edit); r.Code != "applied" {
		t.Fatal(r)
	}
	if r = e.Execute(t.Context(), "stale", edit); r.Code != "stale" {
		t.Fatal("CAS bypassed", r)
	}
	data, err := os.ReadFile(filepath.Join(e.Files.Root(), request.Path))
	if err != nil || string(data) != "after\n" {
		t.Fatal("actual file mismatch", err)
	}
}
