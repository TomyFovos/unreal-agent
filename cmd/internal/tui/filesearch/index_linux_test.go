//go:build linux

package filesearch

import (
	"context"
	"testing"
)

func TestScanRejectsInvalidUTF8Filename(t *testing.T) {
	// Linux filesystems admit byte names that APFS cannot create. Keep the real
	// directory-entry rejection covered as well as the portable SafePath tests.
	root := t.TempDir()
	fixtureFile(t, root, "safe.md", "data")
	fixtureFile(t, root, string([]byte{0xff}), "data")
	r, err := Scan(context.Background(), root, DefaultLimits())
	if err != nil || len(r.Index.files) != 1 || r.Index.files[0] != "safe.md" {
		t.Fatal("invalid filename entered the index", r, err)
	}
}
