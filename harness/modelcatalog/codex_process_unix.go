//go:build linux || darwin

package modelcatalog

import (
	"os/exec"
	"syscall"
	"time"
)

func isolateCodexCatalog(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { stopCodexCatalog(cmd); return nil }
	cmd.WaitDelay = time.Second
}

func stopCodexCatalog(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
