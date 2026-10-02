//go:build linux || darwin

package projectinstructions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func agentsPath(workspace string) string { return filepath.Join(workspace, FileName) }

func requireCode(t *testing.T, err error, want Code) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != want {
		t.Fatalf("error = %v, want code %q", err, want)
	}
}

func TestDiscoverMissingIsNone(t *testing.T) {
	got, err := Discover(t.TempDir())
	if err != nil || got != None() {
		t.Fatalf("Discover = %+v, %v; want none", got, err)
	}
	if err = got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReadsRootAgentsExactly(t *testing.T) {
	workspace := t.TempDir()
	content := "# Rules\n\nUse tabs. 日本語\n"
	if err := os.WriteFile(agentsPath(workspace), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// The operator-chosen workspace may itself be reached through a symlink.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{workspace, link} {
		got, err := Discover(root)
		if err != nil {
			t.Fatal(err)
		}
		if got.SourceKind != SourceWorkspaceAgents || got.SourcePath != FileName || got.Content != content || got.ByteLength != int64(len(content)) || !strings.HasPrefix(got.Digest, "sha256:") {
			t.Fatalf("snapshot = %+v", got)
		}
		if err = got.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverIgnoresOtherInstructionSurfaces(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"CLAUDE.md", "GEMINI.md", "RULES.md", "agents.md"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("ignored"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", FileName), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(workspace)
	if err != nil {
		t.Fatal(err)
	}
	// agents.md matches on case-insensitive filesystems (default macOS).
	if got != None() && got.Content != "ignored" {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestDiscoverRejectsSymlinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"escape": outside, "inside": "README.md", "dangling": "missing.md"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("inside"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, agentsPath(workspace)); err != nil {
				t.Fatal(err)
			}
			_, err := Discover(workspace)
			requireCode(t, err, CodeSymlink)
		})
	}
}

func TestDiscoverRejectsNonRegularFiles(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.Mkdir(agentsPath(workspace), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Discover(workspace)
		requireCode(t, err, CodeNotRegular)
	})
	t.Run("fifo", func(t *testing.T) {
		workspace := t.TempDir()
		if err := unix.Mkfifo(agentsPath(workspace), 0o644); err != nil {
			t.Fatal(err)
		}
		// O_NONBLOCK keeps discovery from waiting for a writer.
		_, err := Discover(workspace)
		requireCode(t, err, CodeNotRegular)
	})
	t.Run("hard-link", func(t *testing.T) {
		workspace := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(outside, agentsPath(workspace)); err != nil {
			t.Skip("hard links unavailable:", err)
		}
		_, err := Discover(workspace)
		requireCode(t, err, CodeNotRegular)
	})
}

func TestDiscoverRejectsOversizedWithoutTruncating(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(agentsPath(workspace), []byte(strings.Repeat("a", MaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(workspace)
	requireCode(t, err, CodeOversized)
	if got != (Snapshot{}) {
		t.Fatalf("oversized file produced a snapshot: %d bytes", got.ByteLength)
	}
	if err = os.WriteFile(agentsPath(workspace), []byte(strings.Repeat("a", MaxBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = Discover(workspace); err != nil || got.ByteLength != MaxBytes {
		t.Fatalf("limit-sized file: %d, %v", got.ByteLength, err)
	}
}

func TestDiscoverRejectsBinaryAndInvalidEncoding(t *testing.T) {
	for name, tc := range map[string]struct {
		content []byte
		code    Code
	}{
		"nul":     {[]byte("text\x00more"), CodeBinary},
		"png":     {[]byte("\x89PNG\r\n\x1a\n\x00\x00"), CodeBinary},
		"latin-1": {[]byte("caf\xe9"), CodeInvalidEncoding},
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(agentsPath(workspace), tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Discover(workspace)
			requireCode(t, err, tc.code)
		})
	}
}

func TestDiscoverReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes")
	}
	workspace := t.TempDir()
	if err := os.WriteFile(agentsPath(workspace), []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(workspace)
	requireCode(t, err, CodeUnreadable)
}

func TestDiscoverRejectsInvalidWorkspace(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{"relative", filepath.Join(t.TempDir(), "missing"), file} {
		_, err := Discover(workspace)
		requireCode(t, err, CodeInvalidWorkspace)
	}
}
