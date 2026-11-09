package claudeagent

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

const maxCompactStateBytes = 4 << 10

// compactContext restores Host-owned state rather than relying on a summary
// of old child reports or the permission mode at process startup.
func (t *turn) compactContext() (text string) {
	defer func() {
		if len(text) > maxCompactStateBytes {
			text = clip(text, maxCompactStateBytes)
		}
	}()
	mode := modeLabel(permission.ControllerFromContext(t.ctx))
	if mode == "" {
		mode = "set by the operator"
	}
	cli := t.subagents.Binary
	if cli == "" {
		cli = "unreal-agent"
	} else if !literalCLI(cli) {
		cli = "'" + cli + "'"
	}
	fallback := "Check children with " + cli + " subagent list."
	state := fmt.Sprintf("Unreal Agent current state after compaction:\nPermission mode: %s.\n", mode)
	if t.subagents.Binary == "" || t.opt.Identity.SessionID == "" {
		return state + fallback
	}
	cmd := exec.CommandContext(t.ctx, t.subagents.Binary, "subagent", "list")
	cmd.Dir, cmd.Env = t.directory, t.environ
	output := &boundedText{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	isolate(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	if cmd.Run() != nil {
		return state + fallback
	}
	state += "Active children (handle template state):\n"
	found, parsed := false, false
	for line := range strings.SplitSeq(output.text.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ended:") {
			parsed = true
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		for _, field := range fields[2:] {
			status := operation.Status(field)
			switch status {
			case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
				parsed = true
			case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
				parsed, found = true, true
				summary := fmt.Sprintf("%s %s %s\n", fields[0], fields[1], status)
				if len(state)+len(summary)+len(fallback) > maxCompactStateBytes {
					return state + fallback
				}
				state += summary
			default:
				continue
			}
			break
		}
	}
	if !parsed && strings.TrimSpace(output.text.String()) != "No children in this Session." {
		return state + fallback
	}
	if !found {
		state += "None.\n"
	}
	if output.truncated {
		state += fallback
	}
	return state
}

// Keep only a prefix while draining stdout; child reports beyond the needed
// handle/template/status are never included in the restored context.
type boundedText struct {
	text      strings.Builder
	limit     int
	truncated bool
}

func (b *boundedText) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, b.limit-b.text.Len())
	b.text.Write(p[:keep])
	b.truncated = b.truncated || keep < n
	return n, nil
}
