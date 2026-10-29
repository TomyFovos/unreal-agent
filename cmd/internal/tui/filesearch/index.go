// Package filesearch indexes workspace-relative filenames, never file contents.
// It is a disposable UI aid, not a tool executor or a permission grant.
package filesearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Index struct{ files []string }
type Result struct {
	Index   Index
	Entries int
}

var ErrWorkspace = errors.New("file search workspace unavailable")
var errIgnore = errors.New("file search ignore rules unavailable")

func SafePath(p string) bool {
	if !utf8.ValidString(p) || p == "" || strings.Contains(p, "\\") && filepath.Separator == '\\' || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool {
		return unicode.IsControl(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c || r == 0x2028 || r == 0x2029
	})
}

func protected(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case ".git", ".hg", ".svn", ".ssh", ".aws", ".azure", ".codex", ".claude", ".gnupg", ".kube", ".docker", ".handoff", ".harness", "auth.json", "credentials", "credentials.json", "credentials.yaml", "credentials.yml", "credentials.toml", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".envrc", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "service-account.json":
		return true
	}
	return n == ".env" || strings.HasPrefix(n, ".env.") || strings.HasSuffix(n, ".pem") || strings.HasSuffix(n, ".key") || strings.HasPrefix(n, "credentials.")
}

func openWorkspace(root string) (*os.Root, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWorkspace, err)
	}
	return r, nil
}

func Scan(ctx context.Context, workspace string) (Result, error) {
	var result Result
	r, err := openWorkspace(workspace)
	if err != nil {
		return result, err
	}
	defer r.Close()
	var visit func(string, []ignoreRule) error
	visit = func(dir string, inherited []ignoreRule) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rules := inherited
		ignoreFiles := []string{path.Join(dir, ".ignore"), path.Join(dir, ".gitignore")}
		if dir == "." {
			if info, err := r.Lstat(".git"); err == nil && info.IsDir() {
				ignoreFiles = append([]string{".git/info/exclude"}, ignoreFiles...)
			}
		}
		for _, name := range ignoreFiles {
			loaded, _, err := readRules(r, name, dir)
			if err != nil {
				return fmt.Errorf("read ignore %s: %w", name, err)
			}
			if len(loaded) > 0 {
				rules = append(append([]ignoreRule(nil), rules...), loaded...)
			}
		}
		f, err := r.Open(filepath.FromSlash(dir))
		if err != nil {
			return fmt.Errorf("open directory %s: %w", dir, err)
		}
		var entries []fs.DirEntry
		for {
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return err
			}
			batch, err := f.ReadDir(128)
			result.Entries += len(batch)
			entries = append(entries, batch...)
			if err != nil {
				_ = f.Close()
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("read directory %s: %w", dir, err)
				}
				break
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := path.Join(dir, entry.Name())
			if !SafePath(name) || protected(entry.Name()) || entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			info, err := r.Lstat(filepath.FromSlash(name))
			if err != nil {
				return fmt.Errorf("stat %s: %w", name, err)
			}
			if info.Mode()&fs.ModeSymlink != 0 || ignored(rules, name, info.IsDir()) {
				continue
			}
			if info.IsDir() {
				if err := visit(name, rules); err != nil {
					return err
				}
			} else if info.Mode().IsRegular() {
				result.Index.files = append(result.Index.files, name)
			}
		}
		return nil
	}
	if err := visit(".", nil); err != nil {
		return Result{}, err
	}
	sort.Strings(result.Index.files)
	return result, nil
}
