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

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"golang.org/x/sys/unix"
)

func TestHumanPermissionRealTerminalConfirmationOnceDenyDetach(t *testing.T) {
	if socket := os.Getenv("UA_PERMISSION_PTY_HELPER"); socket != "" {
		if err := run(t.Context(), []string{"attach", "--socket", socket, "--session", "human-terminal"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := privateCLIDirectory(t)
	owner, err := host.New(t.Context(), host.Config{Directory: filepath.Join(dir, "sessions"), PermissionMaximum: permission.Unrestricted(), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: terminalModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	s, err := owner.Create(t.Context(), host.Options{ID: "human-terminal", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "host.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- gateway.ListenAndServe(ctx, socket, gateway.Config{Host: owner, Policy: permission.Unrestricted()})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := gateway.NewClient(socket)
	defer client.Close()
	waitInteractiveContext(t, func(ctx context.Context) bool { _, err := client.Methods(ctx); return err == nil })
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
	if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 28, Col: 100}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHumanPermissionRealTerminalConfirmationOnceDenyDetach$")
	cmd.Env = append(os.Environ(), "UA_PERMISSION_PTY_HELPER="+socket, "NO_COLOR=1")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
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
				t.Fatalf("PTY: %v", e)
			}
			if time.Now().After(deadline) {
				t.Fatalf("PTY missing %q: %s", text, output.String())
			}
			time.Sleep(time.Millisecond)
		}
	}
	send := func(text string) {
		t.Helper()
		output.Reset()
		if _, err := unix.Write(fd, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	readUntil("Permissions: Read Only")
	send("/permissions\r")
	readUntil("Schemas are capabilities")
	send("\x1b[B\x1b[B\r")
	readUntil("NO OS SANDBOX")
	v, _ := client.Inspect(t.Context(), s.ID, 0, 128)
	if v.Permissions.Mode != permission.ReadOnly {
		t.Fatal("selection approved Full")
	}
	send("\x1b[B\r")
	readUntil("Permissions: Full Access")
	send("/permissions\r")
	readUntil("Schemas are capabilities")
	send("\x1b[A\r")
	readUntil("Permissions: Ask for Approval")
	root := t.TempDir()
	spec, err := operation.NewShellSpec(operation.ShellInput{Shell: "/bin/sh", Directory: root, Command: "printf once >> marker"}, t.TempDir(), 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		id := fmt.Sprint("pty-", allow)
		if _, err = s.SubmitOperation(t.Context(), s.Generation, inbox.ID(id), operation.ID(id), "Bash", spec); err != nil {
			t.Fatal(err)
		}
		readUntil("Allow once")
		readUntil("$ printf once >> marker")
		if allow {
			send("\r")
		} else {
			send("\x1b[B\r")
		}
		end := inspectInteractive(t, client, s.ID, func(v host.View) bool {
			for _, op := range v.Operations {
				if op.ID == operation.ID(id) {
					return op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed
				}
			}
			return false
		})
		for _, op := range end.Operations {
			if op.ID == operation.ID(id) && ((op.Denial == nil) != allow) {
				t.Fatal("PTY decision not canonical", op)
			}
		}
		readUntil("Permission decision recorded")
	}
	value, err := os.ReadFile(filepath.Join(root, "marker"))
	if err != nil || string(value) != "once" {
		t.Fatal("denied/duplicate execution", err)
	}
	send("/permissions\r")
	readUntil("Schemas are capabilities")
	send("\x1b[B\r")
	readUntil("NO OS SANDBOX")
	send("\x1b[B\r")
	readUntil("Permissions: Full Access")
	send("\x04")
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach hung")
	}
	v, err = client.Inspect(t.Context(), s.ID, 0, 128)
	if err != nil || v.Permissions.Mode != permission.FullAccess || !v.Running {
		t.Fatal("detach lost approved Host Session", err)
	}
}
