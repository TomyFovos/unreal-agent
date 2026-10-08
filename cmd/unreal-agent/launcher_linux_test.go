//go:build linux

package main

import (
	"fmt"
	"net"
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
	"golang.org/x/sys/unix"
)

func launcherPeerPID(socket string) (int, error) {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: socket})
	if err != nil {
		return 0, err
	}
	defer c.Close()
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *unix.Ucred
	var controlErr error
	if err = raw.Control(func(fd uintptr) {
		credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	if credential.Uid != uint32(os.Geteuid()) {
		return 0, fmt.Errorf("test Host is not owned by current user")
	}
	return int(credential.Pid), nil
}

func TestRealUnrealLauncherTTYDetachAndReattach(t *testing.T) {
	// Adopt/reap the real background daemon after its launching TUI exits. This
	// is test cleanup only; production requires no subreaper or service manager.
	var old int32
	_, _, errno := unix.Syscall6(unix.SYS_PRCTL, unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&old)), 0, 0, 0, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(old), 0, 0, 0); err != nil {
			t.Error(err)
		}
	})
	binary := filepath.Join(privateCLIDirectory(t), "unreal")
	build := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build installed unreal: %v\n%s", err, output)
	}
	f := newLauncherFixture(t)
	pid := 0
	t.Cleanup(func() {
		if pid == 0 {
			pid, _ = launcherPeerPID(f.o.socket)
		}
		if pid == 0 {
			return
		}
		if err := unix.Kill(pid, unix.SIGTERM); err != nil {
			t.Error(err)
		}
		done := make(chan error, 1)
		go func() {
			var status unix.WaitStatus
			_, err := unix.Wait4(pid, &status, 0, nil)
			if err == nil && (!status.Exited() || status.ExitStatus() != 0) {
				err = fmt.Errorf("background Host exit: %v", status)
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			unix.Kill(pid, unix.SIGKILL)
			<-done
			t.Error("background Host failed to drain")
		}
	})
	client := gateway.NewClient(f.o.socket)
	defer client.Close()
	var remembered host.View
	for _, name := range []string{"", "test", "test"} {
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
			if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
				t.Fatal(err)
			}
			args := []string{"--config", f.o.config}
			if name != "" {
				args = append(args, name)
			}
			cmd := exec.CommandContext(t.Context(), binary, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			var output strings.Builder
			readUntil := func(text string) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				buffer := make([]byte, 4096)
				for {
					n, e := unix.Read(fd, buffer)
					if n > 0 {
						output.Write(buffer[:n])
					}
					if strings.Contains(output.String(), text) {
						return
					}
					if e != nil && e != unix.EAGAIN && e != unix.EINTR {
						t.Fatal(e)
					}
					if time.Now().After(deadline) {
						t.Fatalf("launcher terminal missing %q: %s", text, output.String())
					}
					time.Sleep(time.Millisecond)
				}
			}
			readUntil("⇄ connected")
			owner, err := launcherPeerPID(f.o.socket)
			if err != nil {
				t.Fatal(err)
			}
			if pid != 0 && pid != owner {
				t.Fatal("reattach duplicated the Host")
			}
			pid = owner
			if sid, e := unix.Getsid(pid); e != nil || sid != pid || pid == cmd.Process.Pid {
				t.Fatal("Host shares TUI lifetime/session", sid, e)
			}
			for n, want := range []string{os.DevNull, filepath.Join(f.o.state, "host.log"), filepath.Join(f.o.state, "host.log")} {
				actual, e := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, n))
				if e != nil || actual != want {
					t.Fatal("Host depends on terminal stdio", actual, e)
				}
			}
			for _, file := range []string{"cmdline", "environ"} {
				data, e := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, file))
				if e != nil {
					t.Fatal(e)
				}
				assertNoCredentialLeak(t, string(data), "launcher-token-sensitive", "launcher-account-sensitive", "ambient-launcher-token-sensitive", "ambient-launcher-account-sensitive")
			}
			if name == "test" && remembered.Generation == "" {
				if _, err = unix.Write(fd, []byte("remember this conversation\r")); err != nil {
					t.Fatal(err)
				}
				readUntil("launcher remembered response")
				remembered = inspectInteractive(t, client, "test", func(v host.View) bool { return responseCount(v) == 1 })
			} else if name == "test" {
				readUntil("launcher remembered response")
			}
			// Both supported detach routes end the TUI, including its real PTY.
			text := "\x04"
			if name == "test" {
				text = "/detach\r"
			}
			if _, err = unix.Write(fd, []byte(text)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("launcher TUI did not detach")
			}
		}()
		if _, err := client.Methods(t.Context()); err != nil {
			t.Fatal("TUI exit stopped the Host", err)
		}
		if name == "test" {
			view, err := client.Inspect(t.Context(), "test", 0, 128)
			if err != nil || !view.Running || view.Generation != remembered.Generation || responseCount(view) != 1 {
				t.Fatal("reattach lost same conversation", err)
			}
		}
	}
	if f.calls.Load() != 1 {
		t.Fatal("reattach sent an extra model request")
	}
	child := exec.CommandContext(t.Context(), binary, "child", "--stdio", "--unknown")
	if output, err := child.CombinedOutput(); err == nil || !strings.Contains(string(output), "-unknown") {
		t.Fatal("installed unreal did not dispatch existing child protocol", err, string(output))
	}
	assertNoStoredCredentials(t, f.o.state, "launcher-token-sensitive", "launcher-account-sensitive", "ambient-launcher-token-sensitive", "ambient-launcher-account-sensitive", "unused-external-refresh-secret")
}
