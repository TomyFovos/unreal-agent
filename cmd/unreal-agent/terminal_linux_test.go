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

	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"golang.org/x/sys/unix"
)

type terminalModel struct{}

func (terminalModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "terminal-response 猫"}}}}, nil
}
func TestRealTerminalAttachResizePrivateAuthAndDetach(t *testing.T) {
	if socket := os.Getenv("UA_TUI_PTY_HELPER"); socket != "" {
		if err := run(t.Context(), []string{"attach", "--socket", socket, "--session", "terminal"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir, err := os.MkdirTemp("", "ua-pty-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	owner, err := host.New(t.Context(), host.Config{Directory: filepath.Join(dir, "sessions"), Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: terminalModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	current, err := owner.Create(t.Context(), host.Options{ID: "terminal"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := credential.OpenLocal(filepath.Join(dir, "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "host.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- gateway.ListenAndServe(ctx, socket, gateway.Config{Host: owner, Policy: permission.Unrestricted(), Auth: authflow.New(credential.NewManager(store, nil))})
	}()
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			t.Error(err)
		}
	}()
	client := gateway.NewClient(socket)
	defer client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := client.Methods(t.Context()); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not ready")
		}
		time.Sleep(time.Millisecond)
	}
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
	if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 100}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRealTerminalAttachResizePrivateAuthAndDetach$")
	cmd.Env = append(os.Environ(), "UA_TUI_PTY_HELPER="+socket)
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
				t.Fatalf("terminal read %v: %s", e, output.String())
			}
			if time.Now().After(deadline) {
				t.Fatalf("terminal missing %q: %s", text, output.String())
			}
			time.Sleep(time.Millisecond)
		}
	}
	send := func(text string) {
		t.Helper()
		if _, e := unix.Write(fd, []byte(text)); e != nil {
			t.Fatal(e)
		}
	}
	readUntil("connected")
	send("こんにちは\r")
	readUntil("terminal-response")
	send("/login openai terminal\r")
	readUntil("private: API key for openai/terminal")
	send("never-display-this-key\r")
	readUntil("API key stored")
	if strings.Contains(output.String(), "never-display-this-key") {
		t.Fatal("raw terminal echoed private key")
	}
	if err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 8, Col: 16}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	send("\x04")
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case e := <-waited:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach hung")
	}
	select {
	case <-current.Done():
		t.Fatal("terminal exit stopped owner")
	default:
	}
	entries, err := client.ListCredentials(t.Context())
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
}
