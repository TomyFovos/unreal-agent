//go:build !linux && !darwin

package claudecode

import "os/exec"

func isolateProcess(*exec.Cmd) error          { return &Error{Code: "unsupported_version"} }
func killProcessGroup(*exec.Cmd) error        { return nil }
func terminatedByOwner(error) bool            { return false }
func checkPolicySources(string, string) error { return &Error{Code: "policy_isolation_unavailable"} }
