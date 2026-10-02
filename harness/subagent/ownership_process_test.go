package subagent_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

type resumeOutcome struct {
	ID         session.ID
	Owned      bool
	Generation string
}

func TestChildResumeContenderHelper(t *testing.T) {
	dir := os.Getenv("UNREAL_RESUME_CONTENDER")
	if dir == "" {
		return
	}
	// Both processes wait at this barrier before calling the actual Host gate.
	var signal [1]byte
	if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
		os.Exit(2)
	}
	id := sub.ChildID("parent", "start")
	h, err := host.New(context.Background(), host.Config{Directory: dir, Build: factory(sub.Config{}, adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, nil
	}))})
	if err != nil {
		os.Exit(3)
	}
	child, err := h.Resume(context.Background(), host.Options{ID: id, Lifecycle: "child", Policy: permission.Unrestricted()})
	out := resumeOutcome{ID: id, Owned: errors.Is(err, localfile.ErrWriterOwned)}
	if err == nil {
		out.Generation = child.Generation
	} else if !out.Owned {
		os.Exit(4)
	}
	data, _ := json.Marshal(out)
	os.Stdout.Write(append(data, '\n'))
	if err == nil {
		io.Copy(io.Discard, os.Stdin)
	}
	h.Close()
	os.Exit(0)
}

func TestSimultaneousChildResumeUsesProcessSharedOwnership(t *testing.T) {
	dir := t.TempDir()
	id := sub.ChildID("parent", "start")
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type contender struct {
		cmd     *exec.Cmd
		gate    io.WriteCloser
		outcome <-chan resumeOutcome
	}
	var contenders []contender
	for range 2 {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestChildResumeContenderHelper$")
		cmd.Env = append(os.Environ(), "UNREAL_RESUME_CONTENDER="+dir)
		gate, e := cmd.StdinPipe()
		if e != nil {
			t.Fatal(e)
		}
		out, e := cmd.StdoutPipe()
		if e != nil {
			t.Fatal(e)
		}
		cmd.Stderr = os.Stderr
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { gate.Close(); cmd.Process.Kill(); cmd.Wait() })
		result := make(chan resumeOutcome, 1)
		go func() {
			line, e := bufio.NewReader(out).ReadBytes('\n')
			var r resumeOutcome
			if e == nil && json.Unmarshal(line, &r) == nil {
				result <- r
			}
			close(result)
		}()
		contenders = append(contenders, contender{cmd, gate, result})
	}
	for _, c := range contenders {
		if _, err = c.gate.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	winners, losers := 0, 0
	for _, c := range contenders {
		select {
		case result, ok := <-c.outcome:
			if !ok || result.ID != id {
				t.Fatal("contender identity/outcome", result)
			}
			if result.Owned {
				losers++
			} else if result.Generation != "" {
				winners++
			} else {
				t.Fatal(result)
			}
		case <-ctx.Done():
			t.Fatal("simultaneous resume deadline")
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatal("writers/typed losers", winners, losers)
	}
	if lock, e := store.AcquireWriter(id); !errors.Is(e, localfile.ErrWriterOwned) {
		if lock != nil {
			lock.Close()
		}
		t.Fatal("winner did not hold writer", e)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.session.jsonl"))
	if err != nil || len(entries) != 1 {
		t.Fatal("second child or canonical mutation", entries, err)
	}
	for _, c := range contenders {
		c.gate.Close()
		if err = c.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := store.AcquireWriter(id)
	if err != nil {
		t.Fatal("writer not released", err)
	}
	lock.Close()
}
