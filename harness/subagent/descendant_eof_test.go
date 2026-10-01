package subagent_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestDescendantChildHelper(t *testing.T) {
	if os.Getenv("UNREAL_DESCENDANT_CHILD") == "" {
		return
	}
	err := sub.Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, c sub.ChildConfig, send sub.Sender) (*host.Session, io.Closer, error) {
		model := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
			return llm.Response{}, nil
		})
		h, err := host.New(ctx, host.Config{Directory: c.SessionDirectory, Build: factory(sub.Config{Child: &c, SendParent: send}, model)})
		if err != nil {
			return nil, nil, err
		}
		raw, _ := json.Marshal(c)
		child, err := h.Open(ctx, host.Options{ID: c.ChildID, Lifecycle: "child", Configuration: raw, Initial: []inbox.Input{c.InitialInput()}, Policy: permission.Unrestricted()})
		if err != nil {
			h.Close()
			return nil, nil, err
		}
		// Escape the child's process group and explicitly inherit all IPC FDs.
		// It must remain alive while EOF tears down the child runtime/lock.
		descendant := exec.Command("/bin/sleep", "60")
		descendant.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		descendant.Stdin, descendant.Stdout, descendant.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = descendant.Start(); err != nil {
			h.Close()
			return nil, nil, err
		}
		if err = os.WriteFile(filepath.Join(c.SessionDirectory, "descendant.pid"), []byte(strconv.Itoa(descendant.Process.Pid)), 0600); err != nil {
			descendant.Process.Kill()
			h.Close()
			return nil, nil, err
		}
		return child, closeFunc(func() error {
			h.Close()
			return os.WriteFile(filepath.Join(c.SessionDirectory, "child.closed"), []byte("closed"), 0600)
		}), nil
	})
	if err == nil {
		os.Exit(3)
	} // Owner EOF is an explicit transport failure.
	os.Exit(0)
}

func TestDescendantParentHelper(t *testing.T) {
	dir := os.Getenv("UNREAL_DESCENDANT_PARENT")
	if dir == "" {
		return
	}
	binary, _ := os.Executable()
	template := sub.Template{Workspace: dir, Runtime: jsontext.Value("{}"), Policy: permission.Config{Tools: []string{"Finish"}}}
	model := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	})
	h, err := host.New(context.Background(), host.Config{Directory: dir, Build: factory(sub.Config{Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestDescendantChildHelper$"}, Environment: append(os.Environ(), "UNREAL_DESCENDANT_CHILD=1"), Templates: map[string]sub.Template{"default": template}}, model)})
	if err != nil {
		os.Exit(2)
	}
	s, err := h.Create(context.Background(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
	if err != nil {
		os.Exit(3)
	}
	spec, _ := sub.NewSpec(sub.Plan{Version: 1, Action: "start", ParentID: s.ID, Template: "default", Configuration: &template, Text: "wait"})
	if _, err = s.SubmitOperation(context.Background(), s.Generation, "spawn", "start", "SubagentStart", spec); err != nil {
		os.Exit(4)
	}
	waitReady(t, s)
	if err = os.WriteFile(filepath.Join(dir, "parent.ready"), []byte("ready"), 0600); err != nil {
		os.Exit(5)
	}
	select {}
}

func TestParentDeathEOFCleanupWithSurvivingDescriptorDescendant(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDescendantParentHelper$")
	cmd.Env = append(os.Environ(), "UNREAL_DESCENDANT_PARENT="+dir)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	awaitBoundary(t, func() bool { _, err := os.Stat(filepath.Join(dir, "parent.ready")); return err == nil })
	data, err := os.ReadFile(filepath.Join(dir, "descendant.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err = syscall.Kill(pid, 0); err != nil {
		t.Fatal("descendant not alive before owner loss", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	// Three seconds is below the ordinary five-second force-kill grace period.
	deadline, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err = os.Stat(filepath.Join(dir, "child.closed")); err == nil {
			break
		}
		select {
		case <-deadline.Done():
			t.Fatal("IPC EOF cleanup hung with inherited descriptor")
		case <-ticker.C:
		}
	}
	if err = syscall.Kill(pid, 0); err != nil {
		t.Fatal("fixture did not retain a surviving descendant", err)
	}
	store, _ := localfile.New(dir)
	lock, err := store.AcquireWriter(sub.ChildID("parent", "start"))
	if err != nil {
		t.Fatal("EOF leaked writer ownership", err)
	}
	lock.Close()
}
