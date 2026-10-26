//go:build !(linux || darwin)

package claudeagent

import (
	"os/exec"
	"time"
)

func isolate(cmd *exec.Cmd) { cmd.WaitDelay = 5 * time.Second }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
