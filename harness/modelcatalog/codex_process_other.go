//go:build !linux && !darwin

package modelcatalog

import (
	"os/exec"
	"time"
)

func isolateCodexCatalog(cmd *exec.Cmd) { cmd.WaitDelay = time.Second }
func stopCodexCatalog(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
