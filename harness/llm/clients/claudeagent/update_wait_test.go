//go:build linux || darwin

package claudeagent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type cliUpdateFixture struct {
	*harness
	target, executable string
	progress           chan llm.Progress
	done               chan error
}

func newCLIUpdateFixture(t *testing.T) *cliUpdateFixture {
	t.Helper()
	h := newHarness(t, "echo")
	dir := t.TempDir()
	f := &cliUpdateFixture{harness: h, target: filepath.Join(dir, "claude-target"), executable: h.client.config.Binary,
		progress: make(chan llm.Progress, 16), done: make(chan error, 1)}
	h.client.config.Binary = filepath.Join(dir, "claude")
	if err := os.Symlink(filepath.Base(f.target), h.client.config.Binary); err != nil {
		t.Fatal(err)
	}
	h.client.config.Environ = appendEnv(h.client.config.Environ, "CLAUDEAGENT_TEST_BINARY="+f.executable)
	return f
}

func (f *cliUpdateFixture) start(ctx context.Context) {
	go func() {
		response, err := f.client.Respond(ctx, llm.Request{Input: []llm.Item{user("test update")}}, llm.RequestOptions{
			Identity: llm.RequestIdentity{SessionID: "update-test"}, Tools: f.tools,
			Progress: func(p llm.Progress) { f.progress <- p },
		})
		if err == nil && (len(response.Output) != 1 || response.Output[0].Data.(llm.Message).Text != "fresh:test update") {
			err = errors.New("fake Claude did not finish the turn with the original prompt")
		}
		f.done <- err
	}()
}

func (f *cliUpdateFixture) expectWait(t *testing.T) {
	t.Helper()
	synctest.Wait()
	select {
	case p := <-f.progress:
		if p.Phase != cliUpdateWaitPhase || p.Delta != "waiting for Claude Code to finish updating" {
			t.Fatalf("wait progress = %+v", p)
		}
	default:
		t.Fatal("Claude Code update wait was not published")
	}
}

func (f *cliUpdateFixture) expectPending(t *testing.T) {
	t.Helper()
	synctest.Wait()
	if _, err := os.Stat(f.args); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CLI started before the file stabilized: %v", err)
	}
	select {
	case err := <-f.done:
		t.Fatalf("turn ended during the wait: %v", err)
	default:
	}
}

func (f *cliUpdateFixture) install(t *testing.T) {
	t.Helper()
	if err := os.Symlink(f.executable, f.target); err != nil {
		t.Fatal(err)
	}
}

func (f *cliUpdateFixture) write(t *testing.T, script string) {
	t.Helper()
	if err := os.WriteFile(f.target, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func (f *cliUpdateFixture) expectSuccess(t *testing.T) {
	t.Helper()
	if err := <-f.done; err != nil {
		t.Fatal(err)
	}
	if p := <-f.progress; p.Phase != "" || !p.Reset || p.Delta != "" {
		t.Fatalf("wait did not clear: %+v", p)
	}
	for len(f.progress) != 0 {
		if p := <-f.progress; p.Phase == cliUpdateWaitPhase {
			t.Fatal("the same wait was published twice")
		}
	}
}

const launchFakeClaude = "#!/bin/sh\nexec \"$CLAUDEAGENT_TEST_BINARY\" \"$@\"\n"

func TestClaudeCodeUpdateWaitRecoversDanglingSymlink(t *testing.T) {
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	synctest.Test(t, func(t *testing.T) {
		f := newCLIUpdateFixture(t)
		f.start(t.Context())
		f.expectWait(t)
		synctest.Sleep(3 * time.Second)
		f.install(t)
		synctest.Sleep(2 * time.Second) // Seen at 4 s, but not yet stable.
		f.expectPending(t)
		f.expectSuccess(t)
		logs := log.String()
		if strings.Count(logs, cliUpdateWaitText) != 1 || strings.Count(logs, "Claude Code update wait ended") != 1 ||
			strings.Count(logs, "path="+f.client.config.Binary) != 2 || !strings.Contains(logs, "duration=") || !strings.Contains(logs, "outcome=ready") {
			t.Fatalf("wait log = %s", logs)
		}
	})
}

func TestClaudeCodeUpdateWaitRequiresStableSizeAndMtime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCLIUpdateFixture(t)
		f.start(t.Context())
		f.expectWait(t)
		f.write(t, launchFakeClaude)
		mtime := time.Unix(946684800, 0)
		setMtime := func(at time.Time) {
			t.Helper()
			if err := os.Chtimes(f.target, at, at); err != nil {
				t.Fatal(err)
			}
		}
		setMtime(mtime)
		synctest.Sleep(2*time.Second + time.Millisecond)
		f.expectPending(t)
		f.write(t, launchFakeClaude+"# appended by the updater\n")
		setMtime(mtime) // Size alone must restart the two-check window.
		synctest.Sleep(2 * time.Second)
		f.expectPending(t)
		setMtime(mtime.Add(time.Second)) // So must mtime alone.
		synctest.Sleep(2 * time.Second)
		f.expectPending(t)
		f.expectSuccess(t)
	})
}

func TestClaudeCodeUpdateWaitRecoversTextFileBusy(t *testing.T) {
	if runtime.GOOS != "linux" {
		if !temporarilyUnusableCLI(syscall.ETXTBSY, t.TempDir()) {
			t.Fatal("ETXTBSY was not classified as temporary")
		}
		return
	}
	synctest.Test(t, func(t *testing.T) {
		f := newCLIUpdateFixture(t)
		f.write(t, launchFakeClaude)
		writer, err := os.OpenFile(f.target, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		if err := exec.Command(f.client.config.Binary).Run(); !errors.Is(err, syscall.ETXTBSY) {
			t.Fatalf("expected real ETXTBSY while the file is held open: %v", err)
		}
		f.start(t.Context())
		f.expectWait(t)
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		f.expectSuccess(t)
	})
}

func TestClaudeCodeUpdateWaitChecksFirstFrameAndLoaderFailure(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		wait                 bool
	}{
		{"loader failure", "", "error while loading shared libraries: libclaude.so: cannot open shared object file", true},
		{"frame already sent", `echo '{"type":"system","subtype":"init"}'` + "\n", "error while loading shared libraries: libclaude.so: cannot open shared object file", false},
		{"ordinary early exit", "", "configuration file not found", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCLIUpdateFixture(t)
				f.write(t, "#!/bin/sh\n"+tc.stdout+`echo "$0: `+tc.stderr+`" >&2`+"\nexit 127\n")
				f.start(t.Context())
				if tc.wait {
					f.expectWait(t)
					f.write(t, launchFakeClaude)
					f.expectSuccess(t)
					return
				}
				var failure *Error
				if err := <-f.done; !errors.As(err, &failure) || failure.Code != "subprocess_failure" || len(f.progress) != 0 {
					t.Fatalf("exit should keep the existing failure: %v", err)
				}
			})
		})
	}
}

func TestClaudeCodeUpdateWaitTimesOutWithOriginalCLIMissingError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCLIUpdateFixture(t)
		f.client.updateWaitLimit = 5 * time.Second
		execErr := exec.Command(f.client.config.Binary).Run()
		if !errors.Is(execErr, os.ErrNotExist) {
			t.Fatal(execErr)
		}
		start := time.Now()
		f.start(t.Context())
		f.expectWait(t)
		var failure *Error
		if err := <-f.done; !errors.As(err, &failure) || failure.Code != "cli_missing" || failure.Message != execErr.Error() {
			t.Fatalf("timeout changed the launch error: %v", err)
		}
		if elapsed := time.Since(start); elapsed != f.client.updateWaitLimit {
			t.Fatalf("wait lasted %s", elapsed)
		}
	})
}

func TestClaudeCodeUpdateWaitCancelsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCLIUpdateFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.start(ctx)
		f.expectWait(t)
		before := time.Now()
		cancel()
		if err := <-f.done; !errors.Is(err, context.Canceled) || time.Since(before) != 0 {
			t.Fatalf("wait did not cancel immediately: %v after %v", err, time.Since(before))
		}
	})
}
