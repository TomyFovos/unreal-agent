package projectinstructions

import (
	"errors"
	"os"
	"path/filepath"
)

// Discover reads the workspace root's AGENTS.md. Absence means no instructions.
func Discover(workspace string) (Snapshot, error) {
	if !filepath.IsAbs(workspace) {
		return Snapshot{}, &Error{Code: CodeInvalidWorkspace, Path: workspace, Err: errors.New("workspace must be absolute")}
	}
	path := filepath.Join(workspace, FileName)
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return None(), nil
	}
	if err != nil {
		return Snapshot{}, &Error{Code: CodeUnreadable, Path: path, Err: err}
	}
	return FromContent(content)
}
