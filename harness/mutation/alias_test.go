//go:build linux || darwin

package mutation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredRootAliasSharesCanonicalTargets(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Root: alias, StateDir: filepath.Join(base, "state"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "a")
	if r := s.Apply(t.Context(), request(path, Revision{}, "one")); r.Code != Applied {
		t.Fatal(r)
	}
	snap, err := s.Snapshot(t.Context(), path)
	if err != nil || string(snap.Data) != "one" || snap.Path != filepath.Join(s.Root(), "a") {
		t.Fatal(snap, err)
	}
	if r := s.Apply(t.Context(), request(snap.Path, snap.Revision, "two")); r.Code != Applied {
		t.Fatal(r)
	}
	if r := s.Apply(t.Context(), request(path, snap.Revision, "stale")); r.Code != Stale {
		t.Fatal(r)
	}
	if r := s.Apply(t.Context(), Request{Version: Version, Changes: []Change{{Path: path}, {Path: snap.Path}}}); r.Code != Invalid {
		t.Fatal(r)
	}
	unconfigured := filepath.Join(base, "unconfigured")
	if err := os.Symlink(real, unconfigured); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Path(filepath.Join(unconfigured, "a")); err == nil {
		t.Fatal("unconfigured alias accepted")
	}
	if err := os.Symlink(real, filepath.Join(real, "nested")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(t.Context(), filepath.Join(alias, "nested", "a")); err == nil {
		t.Fatal("descendant symlink accepted")
	}
}
