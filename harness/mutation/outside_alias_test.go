//go:build linux || darwin

package mutation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// Outside reads resolve symlinked parents (macOS /tmp, /var, /etc) without
// reaching anything the policy, the credential exclusion or the nofollow
// target rule would refuse.
func TestOutsideReadsResolveParentsWithinPolicy(t *testing.T) {
	base := t.TempDir()
	workspace, real, secret := filepath.Join(base, "workspace"), filepath.Join(base, "real"), filepath.Join(base, "secret")
	vault, credentials := filepath.Join(real, "vault"), filepath.Join(real, "nested", ".codex")
	for _, dir := range []string{workspace, secret, vault, credentials} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(real, "data"), filepath.Join(secret, "data"), filepath.Join(vault, "auth.json"), filepath.Join(credentials, "auth.json")} {
		if err := os.WriteFile(file, []byte("needle"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	for link, target := range map[string]string{
		alias:                             real,
		filepath.Join(real, "escape"):     secret,
		filepath.Join(real, ".codex"):     vault,       // only the requested name is a credential path
		filepath.Join(real, "plain"):      credentials, // only the real location is a credential path
		filepath.Join(real, "data-link"):  filepath.Join(real, "data"),
		filepath.Join(workspace, "outer"): real,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := permission.New(permission.Config{ReadRoots: []string{workspace, real}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	ctx := permission.WithPolicy(t.Context(), policy)
	files, err := New(Config{Root: workspace, StateDir: filepath.Join(base, "state"), Authorize: func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	snap, err := files.Snapshot(ctx, filepath.Join(alias, "data"))
	if err != nil || string(snap.Data) != "needle" || snap.Path != filepath.Join(canonical, "data") {
		t.Fatal("aliased outside read", snap.Path, err)
	}
	entries, err := files.ReadDir(ctx, alias)
	if err != nil || len(entries) != 3 || entries[0].Name != "data" || entries[1].Name != "nested" || entries[2].Name != "vault" {
		t.Fatal("aliased outside directory", entries, err)
	}
	if snap, err := files.Snapshot(ctx, filepath.Join(vault, "auth.json")); err != nil || string(snap.Data) != "needle" {
		t.Fatal("ordinary outside read", err)
	}
	// true: the policy or the credential exclusion must refuse it.
	for path, denied := range map[string]bool{
		filepath.Join(real, "escape", "data"):      true, // resolves outside the policy
		filepath.Join(real, ".codex", "auth.json"): true,
		filepath.Join(real, "plain", "auth.json"):  true,
		filepath.Join(real, "data-link"):           false, // the target itself is a symlink
		filepath.Join(workspace, "outer", "data"):  false, // workspace descendants never resolve
	} {
		snap, err := files.Snapshot(ctx, path)
		if err == nil || denied && permission.Failure(err) == nil {
			t.Fatal("read accepted", path, string(snap.Data), err)
		}
	}
	if _, err := files.ReadDir(ctx, filepath.Join(real, "escape")); permission.Failure(err) == nil {
		t.Fatal("directory outside the policy listed", err)
	}
	if _, err := files.Path(filepath.Join(alias, "data")); err == nil {
		t.Fatal("outside alias accepted for mutation")
	}
}
