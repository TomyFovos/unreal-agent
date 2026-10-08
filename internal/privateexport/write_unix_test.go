//go:build linux || darwin

package privateexport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamingPrivateAtomicPublication(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "exports")
	path, err := WriteStream(t.Context(), dir, "session", "md", func(w io.Writer) error {
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 1 || !strings.HasSuffix(files[0].Name(), ".tmp") {
			t.Fatal("completed filename visible before successful write", err)
		}
		info, err := files[0].Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("temporary file not private", err)
		}
		// Incremental writes are not limited to the old response-copy size.
		for range 10 {
			if _, err := io.WriteString(w, strings.Repeat("a", 1<<20)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{dir: 0700, path: 0600} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("incorrect export permissions", err)
		}
	}
	info, _ := os.Stat(path)
	if info.Size() != 10<<20 {
		t.Fatal("stream truncated", info.Size())
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != filepath.Base(path) {
		t.Fatal("temporary name left behind", err)
	}
}

func TestAtomicFailureAndCancelLeaveNoExport(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer_error", true: "canceled"}[cancel], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "exports")
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			sentinel := errors.New("synthetic write failure")
			path, err := WriteStream(ctx, dir, "response", "md", func(w io.Writer) error {
				if _, err := io.WriteString(w, "partial body"); err != nil {
					return err
				}
				if cancel {
					stop()
					return nil
				}
				return sentinel
			})
			if path != "" || err == nil || !cancel && !errors.Is(err, sentinel) || cancel && !errors.Is(err, context.Canceled) {
				t.Fatal("failure lost", err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatal("partial export remained", err)
			}
		})
	}
}

func TestExportNeverOverwritesOrFollowsLinks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "exports")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := "response-collision.md"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("previous complete export"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []bool{false, true} {
		if link {
			if err := os.Symlink(name, filepath.Join(dir, "linked.md")); err != nil {
				t.Fatal(err)
			}
			name = "linked.md"
		}
		_, err := writeStream(t.Context(), dir, name, func(w io.Writer) error {
			_, err := io.WriteString(w, "replacement")
			return err
		})
		if err == nil {
			t.Fatal("existing name replaced")
		}
		data, _ := os.ReadFile(path)
		if string(data) != "previous complete export" {
			t.Fatal("previous file modified")
		}
		if _, err = os.Stat(filepath.Join(dir, "."+name+".tmp")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed publication left temp file", err)
		}
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(t.Context(), dir, "session", "md", nil); err == nil {
		t.Fatal("non-private directory accepted")
	}
	link := filepath.Join(t.TempDir(), "exports-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(t.Context(), link, "response", "md", nil); err == nil {
		t.Fatal("directory symlink accepted")
	}
}

func TestRuntimeConfigPrivateAtomicNoReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config", "unreal-agent")
	path := filepath.Join(dir, "runtime.json")
	data := []byte("{\"Launcher\":{\"Version\":1}}\n")
	if err := CreateRuntimeConfig(t.Context(), path, data); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{path: 0600, dir: 0700} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("runtime config is not private", err)
		}
	}
	if err := CreateRuntimeConfig(t.Context(), path, []byte("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing runtime config overwritten", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(data) {
		t.Fatal("runtime config changed", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != "runtime.json" {
		t.Fatal("runtime temp file left behind", err)
	}
	if err := CreateRuntimeConfig(t.Context(), filepath.Join(dir, "other.json"), data); err == nil {
		t.Fatal("unrelated file publication accepted")
	}
}

func TestRuntimeConfigCancellationAndUnsafeDirectory(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir := filepath.Join(t.TempDir(), "unreal-agent")
	if err := CreateRuntimeConfig(ctx, filepath.Join(dir, "runtime.json"), nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled creation did not fail", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled creation published artifact")
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := CreateRuntimeConfig(t.Context(), filepath.Join(dir, "runtime.json"), nil); err == nil {
		t.Fatal("unsafe runtime directory accepted")
	}
	link := filepath.Join(t.TempDir(), "linked-config")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := CreateRuntimeConfig(t.Context(), filepath.Join(link, "runtime.json"), nil); err == nil {
		t.Fatal("runtime directory symlink followed")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("failed creation left artifact", err)
	}
}

func TestRuntimeConfigAbandonedTempDoesNotBlockNewCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unreal-agent")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(dir, ".runtime.json.tmp")
	if err := os.WriteFile(abandoned, []byte("incomplete previous write"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runtime.json")
	if err := CreateRuntimeConfig(t.Context(), path, []byte("complete new config")); err != nil {
		t.Fatal("abandoned private temp prevented first run", err)
	}
	for p, want := range map[string]string{abandoned: "incomplete previous write", path: "complete new config"} {
		data, err := os.ReadFile(p)
		if err != nil || string(data) != want {
			t.Fatal("atomic creation changed an existing writer's file", err)
		}
	}
}
