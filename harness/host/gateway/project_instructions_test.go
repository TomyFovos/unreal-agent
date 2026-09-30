package gateway

import (
	"fmt"
	"io/fs"
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
