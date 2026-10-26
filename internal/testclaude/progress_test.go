//go:build linux || darwin

package testclaude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSyntheticProgressIsBoundedAndClosed(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"normal", "{\"Phase\":3,\"Generation\":2,\"PID\":1,\"Child\":true}\n{\"Phase\":5,\"Generation\":2,\"PID\":1,\"ExitKnown\":true,\"ExitCode\":7}\n", true},
		{"unknown-phase", "{\"Phase\":6,\"PID\":1}\n", false},
		{"payload", "{\"Phase\":1,\"PID\":1,\"raw\":\"PRIVATE_SYNTHETIC_MARKER\"}\n", false},
		{"truncated", "{\"Phase\":", false},
		{"size", strings.Repeat("x", (256<<10)+1), false},
		{"records", strings.Repeat("{\"Phase\":1,\"PID\":1}\n", 1025), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fixture{Directory: t.TempDir()}
			if err := os.WriteFile(filepath.Join(f.Directory, "structured-progress.jsonl"), []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := f.Progress()
			if (err == nil) != tc.valid {
				t.Fatal("wrong closed progress validation")
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_SYNTHETIC_MARKER") {
				t.Fatal("progress error exposed payload")
			}
			if tc.valid && (got.ChildStarts != 1 || got.ReceiptChecks != 1 || got.ExitIntents != 1 || !got.ExitIntentStatusKnown || got.ExitIntentStatus != 7) {
				t.Fatal("closed progress counters lost evidence")
			}
		})
	}
}

func TestSyntheticProgressRejectsSymlinkAndFIFO(t *testing.T) {
	for _, mode := range []string{"symlink", "fifo", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := &Fixture{Directory: t.TempDir()}
			path := filepath.Join(f.Directory, "structured-progress.jsonl")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "symlink":
				target := filepath.Join(f.Directory, "synthetic-private")
				if err := os.WriteFile(target, []byte("SYNTHETIC_ONLY_CANARY"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			if _, err := f.ProgressContext(ctx); err == nil {
				t.Fatal("unsafe/unavailable progress source accepted")
			}
		})
	}
}
