//go:build linux || darwin

package analysis

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateJSONMarkdownExportsWithoutBodies(t *testing.T) {
	a, now := analysisFixture(t)
	r := a.Snapshot(now, true, false, nil, nil)
	dir := filepath.Join(t.TempDir(), "exports")
	for _, format := range []string{"JSON", "Markdown"} {
		path, err := Export(t.Context(), dir, format, r)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("unsafe export", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if format == "JSON" {
			var decoded map[string]any
			if json.Unmarshal(data, &decoded) != nil || decoded["Version"] != float64(1) {
				t.Fatal("invalid versioned JSON")
			}
		}
		if format == "Markdown" {
			for _, view := range Views[:len(Views)-1] {
				if !strings.Contains(string(data), "## "+view) {
					t.Fatal("missing analysis section", view)
				}
			}
		}
		for _, secret := range []string{"authorization-sensitive", "prompt-sensitive", "input-sensitive", "agent-sensitive", "tool-args-sensitive", "error-credential-sensitive", "operation-sensitive", "provider-credential-sensitive"} {
			if strings.Contains(string(data), secret) {
				t.Fatal("export leaked body/credentials")
			}
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("unsafe directory", err)
	}
	if err = os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = Export(t.Context(), dir, "JSON", r); err == nil {
		t.Fatal("unsafe export directory accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err = os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err = Export(t.Context(), link, "JSON", r); err == nil {
		t.Fatal("symlink directory accepted")
	}
	if _, err = Encode(r, "transcript"); err == nil {
		t.Fatal("enabled unsolicited body export")
	}
}
