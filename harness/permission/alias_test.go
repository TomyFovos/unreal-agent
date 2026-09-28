package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredAliasUsesPinnedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{ReadRoots: []string{alias}, WriteRoots: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	f, err := p.OpenFile(filepath.Join(alias, "a"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	parent, err := p.OpenParent(filepath.Join(alias, "new"))
	if err != nil {
		t.Fatal(err)
	}
	parent.Close()
	if err = os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "other")
	if err = os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if f, err = p.OpenFile(filepath.Join(alias, "secret"), os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Fatal("retargeted alias accepted")
	}
}
