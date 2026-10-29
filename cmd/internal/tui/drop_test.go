package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// A file dragged onto the terminal pastes its Windows path; once sent to the
// sandbox, the path becomes the file's path in the drop directory.
func TestDroppedWindowsPathsMapToTheDropDirectory(t *testing.T) {
	drop := t.TempDir()
	for _, name := range []string{"report.pdf", "画面 1.png"} {
		if err := os.WriteFile(filepath.Join(drop, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, missing := mapDroppedPaths(`これを見て C:\Users\me\Desktop\report.pdf "C:\Users\me\Pictures\画面 1.png" C:\tmp\absent.txt`, drop)
	want := "これを見て " + filepath.Join(drop, "report.pdf") + ` "` + filepath.Join(drop, "画面 1.png") + `" C:\tmp\absent.txt`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if len(missing) != 1 || missing[0] != "absent.txt" {
		t.Fatalf("missing = %q", missing)
	}
	if got, _ := mapDroppedPaths("no paths here", drop); got != "no paths here" {
		t.Fatal(got)
	}
}
