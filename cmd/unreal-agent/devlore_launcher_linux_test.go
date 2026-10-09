//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
	"golang.org/x/sys/unix"
)

func TestDevlorePermanentLauncherNewBashDetachAndReconnect(t *testing.T) {
	kit := os.Getenv("UNREAL_PILOT_KIT_DIRECTORY")
	if kit == "" {
		t.Skip("set UNREAL_PILOT_KIT_DIRECTORY to the existing verified 6284e49 kit")
	}
	f := testclaude.New(t)
	f.Set(t, testclaude.Config{Subscription: "team", Catalog: hostClaudeCatalog, BridgeManagedPermissionsOnly: true,
		StructuredResponses: []string{`{"type":"final","final":{"message":"DEVLORE_FAKE_OK"}}`}})
	home := privateCLIDirectory(t)
	if err := os.Mkdir(filepath.Join(home, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(home, ".local/state/unreal-agent-pilot/devlore/v1/sessions/old-smoke")
	if err := os.MkdirAll(filepath.Dir(old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("immutable old smoke fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	oldInfo, _ := os.Stat(old)
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	packageRoot := privateCLIDirectory(t)
	build := exec.CommandContext(t.Context(), "python3", "-c",
		`import sys; from pathlib import Path; sys.path.insert(0,sys.argv[1]); import devlore_launcher_bundle as b; b.build(Path(sys.argv[2]),"a"*40)`,
		filepath.Join(root, "scripts"), packageRoot)
	build.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build launcher fixture: %v %s", err, output)
	}
	install := exec.CommandContext(t.Context(), "/bin/bash", filepath.Join(packageRoot, "unreal-agent-devlore-launcher-aaaaaaa/install.sh"),
		"--kit-directory", kit, "--claude-binary", f.Binary, "--model", "claude-configured-a", "--effort", "medium", "--policy-audited", "--trust-managed-policy")
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8", "NO_COLOR=1", "PYTHONDONTWRITEBYTECODE=1"}
	install.Env = env
	for range 2 {
		if output, err := install.CombinedOutput(); err != nil {
			t.Fatalf("install permanent launcher: %v %s", err, output)
		}
		// An exec.Cmd is single-use; preserve the same explicit arguments/env.
		install = exec.CommandContext(t.Context(), install.Path, install.Args[1:]...)
		install.Env = env
	}
	config := filepath.Join(home, ".config/unreal-agent-pilot/devlore/launcher-v1/runtime.json")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	configInfo, _ := os.Stat(config)
	for _, flags := range [][]string{{"--noprofile", "-ic"}, {"-lic"}} {
		args := append(flags, `found=$(command -v unreal) && printf '\nCOMMAND_PATH=%s\n' "$found"`)
		probe := exec.CommandContext(t.Context(), "/bin/bash", args...)
		probe.Env = env
		output, err := probe.Output()
		if err != nil || !strings.Contains(string(output), "COMMAND_PATH="+filepath.Join(home, ".local/bin/unreal")) {
			t.Fatal("fresh Bash cannot find permanent unreal", err)
		}
	}
	var oldSubreaper int32
	_, _, errno := unix.Syscall6(unix.SYS_PRCTL, unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&oldSubreaper)), 0, 0, 0, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(oldSubreaper), 0, 0, 0) })
	socket := filepath.Join(home, ".local/state/unreal-agent-pilot/devlore/launcher-v1/run/host.sock")
	client := gateway.NewClient(socket)
	t.Cleanup(func() { _ = client.Close() })
	pid := 0
	t.Cleanup(func() {
		if pid == 0 {
			pid, _ = launcherPeerPID(socket)
		}
		if pid == 0 {
			return
		}
		_ = unix.Kill(pid, unix.SIGTERM)
		done := make(chan error, 1)
		go func() {
			var status unix.WaitStatus
			_, err := unix.Wait4(pid, &status, 0, nil)
			if err == nil && (!status.Exited() || status.ExitStatus() != 0) {
				err = fmt.Errorf("private Host did not drain: %v", status)
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			_ = unix.Kill(pid, unix.SIGKILL)
			<-done
			t.Error("private Host failed to drain")
		}
	})
	var remembered host.View
	for _, name := range []string{"", "devlore-new", "devlore-new"} {
		func() {
			fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 130}); err != nil {
				t.Fatal(err)
			}
			text := "unreal"
			if name != "" {
				text += " " + name
			}
			cmd := exec.CommandContext(t.Context(), "/bin/bash", "--noprofile", "-ic", text)
			cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			var screen strings.Builder
			readUntil := func(marker string) {
				t.Helper()
				deadline := time.Now().Add(15 * time.Second)
				buffer := make([]byte, 4096)
				for time.Now().Before(deadline) {
					n, err := unix.Read(fd, buffer)
					if n > 0 {
						screen.Write(buffer[:n])
					}
					if strings.Contains(screen.String(), marker) {
						return
					}
					if err != nil && err != unix.EAGAIN && err != unix.EINTR {
						t.Fatal("PTY ended before its expected state", marker)
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatal("PTY did not reach its expected state", marker)
			}
			readUntil("connected")
			owner, err := launcherPeerPID(socket)
			if err != nil || pid != 0 && pid != owner {
				t.Fatal("permanent launcher duplicated or lost the Host", err)
			}
			pid = owner
			sid := session.ID(name)
			if sid == "" {
				sid = "default"
			}
			view, err := client.Inspect(t.Context(), sid, 0, 128)
			if err != nil || !view.Running || len(view.Operations) != 0 {
				t.Fatal("new Session not running", err)
			}
			catalog, err := modelExchange(t.Context(), client.Extension, modelRequest{Action: "model.catalog", ID: sid})
			if err != nil || catalog.Catalog == nil || !catalog.Catalog.Authoritative || len(catalog.Catalog.Models) != 3 || len(catalog.Catalog.Models[1].Efforts) != 2 {
				t.Fatal("permanent launcher lost the Claude model/effort catalog", err)
			}
			if name == "devlore-new" && remembered.Generation == "" {
				_, _ = unix.Write(fd, []byte("/model\r"))
				readUntil("Discovered B")
				// Drive the actual picker, waiting for each screen transition
				// before typing into the composer again.
				_, _ = unix.Write(fd, []byte("\x1b[B\r"))
				readUntil("Reasoning effort")
				_, _ = unix.Write(fd, []byte("\x1b[B\r"))
				readUntil("Model changed:")
				_, _ = unix.Write(fd, []byte("offline fake final only\r"))
				readUntil("DEVLORE_FAKE_OK")
				remembered = inspectInteractive(t, client, sid, func(v host.View) bool { return responseCount(v) == 1 })
			} else if name == "devlore-new" {
				readUntil("DEVLORE_FAKE_OK")
				if view.Generation != remembered.Generation || responseCount(view) != 1 {
					t.Fatal("new Bash reconnect lost or duplicated history")
				}
			}
			_, _ = unix.Write(fd, []byte("\x04"))
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("Ctrl-D detach failed", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Ctrl-D did not detach the permanent launcher")
			}
			if _, err = client.Methods(context.Background()); err != nil {
				t.Fatal("detach stopped the private Host", err)
			}
		}()
	}
	if len(f.Calls(t)) != 1 || len(remembered.Operations) != 0 {
		t.Fatal("install/startup/reconnect initiated extra generations or Operations")
	}
	cliArgument(t, f.Calls(t)[0], "--model", "discovered-b")
	cliArgument(t, f.Calls(t)[0], "--effort", "xhigh")
	cliArgument(t, f.Calls(t)[0], "--tools", "")
	cliArgument(t, f.Calls(t)[0], "--allowedTools", "StructuredOutput")
	cliArgument(t, f.Calls(t)[0], "--mcp-config", `{"mcpServers":{}}`)
	after, _ := os.ReadFile(config)
	current, _ := os.Stat(config)
	oldData, _ := os.ReadFile(old)
	oldCurrent, _ := os.Stat(old)
	if string(before) != string(after) || current.ModTime() != configInfo.ModTime() || string(oldData) != "immutable old smoke fixture" || oldCurrent.ModTime() != oldInfo.ModTime() {
		t.Fatal("launcher rewrote startup configuration or old history")
	}
}
