//go:build !linux && !darwin

package projectinstructions

import "errors"

// Discover requires descriptor-relative no-follow opens.
func Discover(workspace string) (Snapshot, error) {
	return Snapshot{}, &Error{Code: CodeUnsupported, Path: FileName, Err: errors.New("project instruction discovery requires Linux or macOS")}
}
