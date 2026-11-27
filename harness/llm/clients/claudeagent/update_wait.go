package claudeagent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const (
	cliUpdatePoll      = 2 * time.Second
	cliUpdateWaitLimit = 15 * time.Minute
	cliUpdateWaitPhase = "claude_code_update_wait"
	cliUpdateWaitText  = "waiting for Claude Code to finish updating"
)

// cliUnavailable wraps the existing launch error until the turn can retry it.
type cliUnavailable struct{ failure *Error }

func (e *cliUnavailable) Error() string { return e.failure.Error() }

func temporarilyUnusableCLI(err error, directory string) bool {
	if errors.Is(err, syscall.ETXTBSY) || errors.Is(err, syscall.ENOEXEC) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrPermission) {
		// Start can report the same errno for chdir. Traversing the directory
		// itself distinguishes that from a missing or inaccessible executable.
		_, dirErr := os.Stat(directory + string(os.PathSeparator) + ".")
		return dirErr == nil
	}
	return false
}

// A loader or launcher can report the exec failure after Start succeeded.
// Require a loader diagnostic: an ordinary early exit is still a turn error.
func executableLoadFailure(state *os.ProcessState, stderr string) bool {
	if state == nil || state.Success() {
		return false
	}
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.Contains(line, ": error while loading shared libraries: ") ||
			strings.HasPrefix(line, "dyld[") && strings.Contains(line, "]: Library not loaded: ") {
			return true
		}
		if state.ExitCode() == 126 || state.ExitCode() == 127 {
			if strings.Contains(line, ": cannot execute binary file") || strings.Contains(line, ": cannot execute: ") {
				return true
			}
			if strings.Contains(line, ": exec: ") || strings.HasPrefix(line, "/usr/bin/env: ") {
				for _, reason := range []string{": not found", ": No such file or directory", ": Text file busy", ": Exec format error", ": Permission denied"} {
					if strings.HasSuffix(line, reason) {
						return true
					}
				}
			}
		}
	}
	return false
}

type cliUpdateWait struct {
	path      string
	remaining time.Duration
	progress  func(llm.Progress)
	started   time.Time
}

func (w *cliUpdateWait) untilReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.started.IsZero() {
		w.started = time.Now()
		slog.Info(cliUpdateWaitText, "path", w.path)
		if w.progress != nil {
			w.progress(llm.Progress{Attempt: 1, Reset: true, Phase: cliUpdateWaitPhase, Delta: cliUpdateWaitText})
		}
	}
	deadline, cancel := context.WithDeadline(ctx, w.started.Add(w.remaining))
	defer cancel()
	poll := time.NewTicker(cliUpdatePoll)
	defer poll.Stop()
	var previous os.FileInfo
	for {
		select {
		case <-deadline.Done():
			return deadline.Err()
		case <-poll.C:
			info, err := os.Stat(w.path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				previous = nil
				continue
			}
			if previous != nil && os.SameFile(previous, info) && previous.Size() == info.Size() && previous.ModTime().Equal(info.ModTime()) {
				return deadline.Err()
			}
			previous = info
		}
	}
}

func (w *cliUpdateWait) finish(outcome string) {
	if w.started.IsZero() {
		return
	}
	duration := time.Since(w.started)
	w.remaining -= duration
	w.started = time.Time{}
	slog.Info("Claude Code update wait ended", "path", w.path, "duration", duration, "outcome", outcome)
	if w.progress != nil {
		w.progress(llm.Progress{Attempt: 1, Reset: true})
	}
}
