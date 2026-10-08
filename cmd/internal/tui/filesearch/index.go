// Package filesearch indexes workspace-relative filenames, never file contents.
// It is a disposable UI aid, not a tool executor or a permission grant.
package filesearch

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Limits struct {
	Files, Entries, Depth, PathBytes, IgnoreBytes, IgnoreTotalBytes, IgnoreRules int
	Duration                                                                     time.Duration
}

func DefaultLimits() Limits {
	return Limits{Files: 10000, Entries: 20000, Depth: 32, PathBytes: 4096,
		IgnoreBytes: 65536, IgnoreTotalBytes: 524288, IgnoreRules: 4096, Duration: time.Second}
}

type Index struct{ files []string }
type Result struct {
	Index   Index
	Entries int
	// A partial index is explicitly identified; unreadable/invalid ignore rules
	// omit their entire subtree rather than exposing ignored names.
	Limited, Incomplete bool
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
	if !filepath.IsAbs(root) {
		return nil, ErrWorkspace
	}
	before, err := os.Lstat(root)
	if err != nil || !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrWorkspace
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, ErrWorkspace
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = r.Close()
		return nil, ErrWorkspace
	}
	return r, nil
}

// Eligible rechecks names at insertion time. All symlinks are excluded, even
// in-workspace links; Root additionally prevents traversal outside the root if
// a directory is replaced concurrently.
func Eligible(workspace, relative string) bool {
	if !SafePath(relative) {
		return false
	}
	r, err := openWorkspace(workspace)
	if err != nil {
		return false
	}
	defer r.Close()
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		if protected(part) {
			return false
		}
		info, err := r.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
		if i == len(parts)-1 {
			return info.Mode().IsRegular()
		}
		if !info.IsDir() {
			return false
		}
	}
	return false
}

func Scan(ctx context.Context, workspace string, limits Limits) (Result, error) {
	parent := ctx
	var result Result
	if limits.Files <= 0 || limits.Entries <= 0 || limits.Depth <= 0 || limits.PathBytes <= 0 || limits.IgnoreBytes <= 0 || limits.IgnoreTotalBytes <= 0 || limits.IgnoreRules <= 0 || limits.Duration <= 0 {
		return result, errors.New("file search limits invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, limits.Duration)
	defer cancel()
	r, err := openWorkspace(workspace)
	if err != nil {
		return result, err
	}
	defer r.Close()
	ignoreBytes, ruleCount := 0, 0
	var visit func(string, int, []ignoreRule) error
	visit = func(dir string, depth int, inherited []ignoreRule) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > limits.Depth {
			result.Limited = true
			return nil
		}
		rules := inherited
		ignoreFiles := []string{path.Join(dir, ".ignore"), path.Join(dir, ".gitignore")}
		if dir == "." {
			if info, err := r.Lstat(".git"); err == nil && info.IsDir() {
				ignoreFiles = append([]string{".git/info/exclude"}, ignoreFiles...)
			}
		}
		for _, name := range ignoreFiles {
			loaded, n, err := readRules(r, name, dir, limits.IgnoreBytes)
			if err != nil {
				result.Incomplete = true
				return nil
			}
			ignoreBytes += n
			ruleCount += len(loaded)
			if ignoreBytes > limits.IgnoreTotalBytes || ruleCount > limits.IgnoreRules {
				result.Limited = true
				return nil
			}
			if len(loaded) > 0 {
				rules = append(append([]ignoreRule(nil), rules...), loaded...)
			}
		}
		f, err := r.Open(filepath.FromSlash(dir))
		if err != nil {
			result.Incomplete = true
			return nil
		}
		var entries []fs.DirEntry
		for {
			if err := ctx.Err(); err != nil {
				_ = f.Close()
				return err
			}
			if result.Entries >= limits.Entries {
				result.Limited = true
				break
			}
			batch, err := f.ReadDir(min(128, limits.Entries-result.Entries))
			result.Entries += len(batch)
			entries = append(entries, batch...)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					result.Incomplete = true
				}
				break
			}
		}
		_ = f.Close()
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := path.Join(dir, entry.Name())
			if !SafePath(name) || protected(entry.Name()) || entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if len(name) > limits.PathBytes {
				result.Limited = true
				continue
			}
			info, err := r.Lstat(filepath.FromSlash(name))
			if err != nil {
				result.Incomplete = true
				continue
			}
			if info.Mode()&fs.ModeSymlink != 0 || ignored(rules, name, info.IsDir()) {
				continue
			}
			if info.IsDir() {
				if result.Entries >= limits.Entries {
					result.Limited = true
					continue
				}
				if err := visit(name, depth+1, rules); err != nil {
					return err
				}
			} else if info.Mode().IsRegular() {
				result.Index.files = append(result.Index.files, name)
				if len(result.Index.files) >= limits.Files {
					result.Limited = true
					return nil
				}
			}
			if len(result.Index.files) >= limits.Files {
				return nil
			}
		}
		return nil
	}
	err = visit(".", 0, nil)
	if parent.Err() != nil {
		return Result{}, parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		result.Limited = true
		// Parent cancellation must discard even a partial index.
	} else if err != nil {
		return Result{}, err
	}
	sort.Strings(result.Index.files)
	return result, nil
}
