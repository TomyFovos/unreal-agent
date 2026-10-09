//go:build linux

package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"golang.org/x/sys/unix"
)

func TestScrollRealPTYWheelKeyboardResizeAndDetach(t *testing.T) {
	if os.Getenv("UA_SCROLL_PTY_HELPER") == "1" {
		var text strings.Builder
		text.WriteString("FIRST_SCROLL_LINE\n")
		for i := range 120 {
			fmt.Fprintf(&text, "scroll-line-%03d 日本語 👩🏽‍💻 é\n", i)
		}
		text.WriteString("LAST_SCROLL_LINE")
		f := conversationFixture(text.String())
		response := f.view.History.Items[len(f.view.History.Items)-1].Data.(sessionstore.ModelResponse)
		response.Response.Generation = &llm.GenerationStatistics{Model: "pty-model", Source: "provider_api", DurationMicros: 2_000_000, Tokens: llm.TokenCounts{Input: 100, Output: 20, InputKnown: true, OutputKnown: true}}
		f.view.History.Items[len(f.view.History.Items)-1].Data = response
		f.view.History.Items = append([]host.HistoryItem{{Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{Provider: "claude-code", Model: "pty-model", Effort: "high", Revision: 1}}}}, f.view.History.Items...)
		for i := range f.view.History.Items {
			f.view.History.Items[i].Sequence = sessionstore.Sequence(i + 1)
		}
		theme := Theme{Plain: true}
		if err := Terminal(t.Context(), Config{Client: f, ID: "s", Theme: &theme}); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.inputs) != 0 {
			t.Fatal("scroll sent a chat/control input")
		}
		return
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
	resize := func(w, h uint16) {
		t.Helper()
		if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: h, Col: w}); err != nil {
			t.Fatal(err)
		}
	}
	resize(100, 24)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestScrollRealPTYWheelKeyboardResizeAndDetach$")
	cmd.Env = append(os.Environ(), "UA_SCROLL_PTY_HELPER=1", "NO_COLOR=1", "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	out := &screenObserver{}
	var pending []byte
	readUntil := func(predicate func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		buffer := make([]byte, 4096)
		for {
			n, err := unix.Read(fd, buffer)
			if n > 0 {
				pending = append(pending, buffer[:n]...)
				// Decode complete differential frames, not historical substrings or
				// split escape/UTF-8 bytes. A resize updates the same visible screen.
				for {
					end := bytes.Index(pending, []byte("\x1b[?25h"))
					if end < 0 {
						break
					}
					end += len("\x1b[?25h")
					out.Write(pending[:end])
					pending = pending[end:]
				}
			}
			if predicate(out.text()) {
				return
			}
			if err != nil && err != unix.EAGAIN && err != unix.EINTR {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("PTY scroll screen predicate not reached:\n%s", out.text())
			}
			time.Sleep(time.Millisecond)
		}
	}
	send := func(wire string) {
		t.Helper()
		if _, err := unix.Write(fd, []byte(wire)); err != nil {
			t.Fatal(err)
		}
	}
	readUntil(func(s string) bool { return strings.Contains(s, "LAST_SCROLL_LINE") })
	if !strings.Contains(out.text(), "Claude/pty-model | high | TPS 10.0(api)") || !strings.Contains(out.text(), "In:100 Out:20") {
		t.Fatal("PTY statistics not below composer", out.text())
	}
	if !strings.Contains(out.rawText(), "\x1b[?1006h") || !strings.Contains(out.rawText(), "\x1b[?1000h") {
		t.Fatal("mouse reporting not enabled")
	}
	send("\x1b[<64;20;5M")
	readUntil(func(s string) bool {
		return strings.Contains(s, "scrollback:") && !strings.Contains(s, "LAST_SCROLL_LINE")
	})
	send("\x1b[1;5H")
	readUntil(func(s string) bool { return strings.Contains(s, "FIRST_SCROLL_LINE") })
	for range 4 {
		send("\x1b[6~")
	}
	readUntil(func(s string) bool {
		return strings.Contains(s, "scroll-line-05") || strings.Contains(s, "scroll-line-06")
	})
	resize(44, 14)
	readUntil(func(s string) bool { return out.cursorX() < 44 && strings.Contains(s, "scrollback:") })
	send("\x1b[1;5H")
	readUntil(func(s string) bool { return strings.Contains(s, "FIRST_SCROLL_LINE") })
	resize(100, 24)
	send("\x1b[1;5F")
	readUntil(func(s string) bool {
		return strings.Contains(s, "LAST_SCROLL_LINE") && !strings.Contains(s, "scrollback:")
	})
	send("\x1b[<64;20;5M")
	readUntil(func(s string) bool { return strings.Contains(s, "scrollback:") })
	send("\x1b[<65;20;5M")
	readUntil(func(s string) bool {
		return strings.Contains(s, "LAST_SCROLL_LINE") && !strings.Contains(s, "scrollback:")
	})
	send("\x04")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach hung")
	}
	// Drain the terminal teardown and require its mouse mode restoration too.
	var tail strings.Builder
	buffer := make([]byte, 4096)
	for {
		n, _ := unix.Read(fd, buffer)
		if n <= 0 {
			break
		}
		tail.Write(buffer[:n])
	}
	tail.Write(pending)
	if !strings.Contains(tail.String(), "\x1b[?1006l") || !strings.Contains(tail.String(), "\x1b[?1000l") {
		t.Fatal("mouse reporting not restored on detach")
	}
}
