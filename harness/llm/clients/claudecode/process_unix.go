//go:build linux || darwin

package claudecode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

func isolateProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Killing the group closes descendant-held pipes too. WaitDelay bounds waits
	// for a misbehaving executable after the context has been canceled.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return nil
}
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// Policy sources are checked by metadata only. --safe-mode deliberately leaves
// managed hooks active, so reject mode refuses such a source before executing Claude.
// Trust mode skips this check and accepts the external administrative boundary.
// No settings, remote policy, keychain or credential contents are opened here.
func checkPolicySources(home, configDir string) error {
	base := "/etc/claude-code"
	if runtime.GOOS == "darwin" {
		base = "/Library/Application Support/ClaudeCode"
	}
	paths := []string{
		filepath.Join(base, "managed-settings.json"),
		filepath.Join(base, "managed-settings.d"),
		filepath.Join(base, "managed-mcp.json"),
		filepath.Join(configDir, "remote-settings.json"),
	}
	if runtime.GOOS == "darwin" {
		paths = append(paths, "/Library/Managed Preferences/com.anthropic.claudecode.plist", filepath.Join(home, "Library/Managed Preferences/com.anthropic.claudecode.plist"))
	}
	return checkPolicyPaths(paths, os.Lstat)
}
