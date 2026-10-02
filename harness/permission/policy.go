// Package permission enforces concrete capabilities at execution boundaries.
// It does not implement human approval or semantic workflow governance.
package permission

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Code string

const (
	Denied      Code = "denied"
	Unsupported Code = "unsupported"
)

// Error is safe to persist and display: it contains no requested URL, path,
// command, headers, environment, or credential values.
type Error struct {
	Code       Code
	Capability string
	Reason     string
}

func (e *Error) Error() string {
	return "permission " + string(e.Code) + ": " + e.Capability + ": " + e.Reason
}
func Failure(err error) *Error {
	var failure *Error
	if errors.As(err, &failure) {
		copy := *failure
		return &copy
	}
	return nil
}
func deny(capability, reason string) error { return &Error{Denied, capability, reason} }

type ProcessMode string

const (
	ProcessDenied       ProcessMode = ""
	ProcessUnrestricted ProcessMode = "unrestricted"
	ProcessSandboxed    ProcessMode = "sandboxed"
)

// Config is copied by New. Zero values deny access. Root directories must exist.
// ProcessUnrestricted permits the process to access the ambient filesystem,
// network, environment and OS resources; it cannot be combined with filesystem
// or network restrictions. No process sandbox is supplied by this package.
type Config struct {
	Tools                  []string
	ReadRoots              []string
	WriteRoots             []string
	NetworkOrigins         []string
	FilesystemUnrestricted bool
	NetworkUnrestricted    bool
	ProcessMode            ProcessMode
}

type root struct {
	path   string
	alias  string
	handle *os.Root
}

// Policy is immutable and safe for concurrent use. Construct it once per Host
// execution ownership lifetime, and Close it after all executors have stopped.
// Its root handles pin authorized directories against path replacement.
type Policy struct {
	all     bool
	config  Config
	reads   []root
	writes  []root
	parents []*Policy
}

func Unrestricted() *Policy { return &Policy{all: true} }
func DenyAll() *Policy      { return &Policy{} }

func New(config Config) (*Policy, error) {
	if config.ProcessMode != ProcessDenied && config.ProcessMode != ProcessUnrestricted && config.ProcessMode != ProcessSandboxed {
		return nil, errors.New("invalid process permission mode")
	}
	config.Tools = slices.Clone(config.Tools)
	config.ReadRoots = slices.Clone(config.ReadRoots)
	config.WriteRoots = slices.Clone(config.WriteRoots)
	config.NetworkOrigins = slices.Clone(config.NetworkOrigins)
	p := &Policy{config: config}
	var err error
	p.reads, err = openRoots(config.ReadRoots)
	if err == nil {
		p.writes, err = openRoots(config.WriteRoots)
	}
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	for i, raw := range config.NetworkOrigins {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			_ = p.Close()
			return nil, errors.New("network origins must be exact HTTP(S) origins without credentials, paths or queries")
		}
		origin, err := originOf(parsed)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.config.NetworkOrigins[i] = origin
	}
	return p, nil
}
func openRoots(paths []string) ([]root, error) {
	var roots []root
	for _, path := range paths {
		original, err := filepath.Abs(path)
		if err != nil {
			return nil, errors.New("invalid permission root")
		}
		canonical, err := filepath.EvalSymlinks(original)
		if err == nil {
			canonical, err = filepath.Abs(canonical)
		}
		if err != nil {
			for _, r := range roots {
				_ = r.handle.Close()
			}
			return nil, errors.New("permission root must be an existing directory")
		}
		handle, err := os.OpenRoot(canonical)
		if err != nil {
			for _, r := range roots {
				_ = r.handle.Close()
			}
			return nil, errors.New("cannot open permission root")
		}
		roots = append(roots, root{path: canonical, alias: original, handle: handle})
	}
	return roots, nil
}

// Close closes only roots owned by this policy. Intersections borrow their
// parents; close each original New policy once its users have stopped.
func (p *Policy) Close() error {
	if p == nil {
		return nil
	}
	var failures []error
	for _, r := range append(slices.Clone(p.reads), p.writes...) {
		failures = append(failures, r.handle.Close())
	}
	return errors.Join(failures...)
}

// Intersect restricts both policies. Neither a child nor a nested operation can
// replace the parent's constraints with an unrestricted policy.
func (p *Policy) Intersect(child *Policy) *Policy {
	if p == nil {
		p = DenyAll()
	}
	if child == nil {
		child = DenyAll()
	}
	return &Policy{parents: []*Policy{p, child}}
}
func (p *Policy) each(check func(*Policy) error) error {
	if p == nil {
		return check(DenyAll())
	}
	if len(p.parents) == 0 {
		return check(p)
	}
	for _, parent := range p.parents {
		if err := parent.each(check); err != nil {
			return err
		}
	}
	return nil
}
func (p *Policy) CheckTool(name string) error {
	return p.each(func(p *Policy) error {
		if p.all || slices.Contains(p.config.Tools, name) {
			return nil
		}
		return deny("tool", "tool is not allowed")
	})
}
func (p *Policy) CheckProcess() error {
	return p.each(func(p *Policy) error {
		if p.all {
			return nil
		}
		if p.config.ProcessMode == ProcessDenied {
			return deny("process", "process creation is not allowed")
		}
		if p.config.ProcessMode != ProcessUnrestricted || !p.config.FilesystemUnrestricted || !p.config.NetworkUnrestricted {
			return &Error{Unsupported, "process", "required filesystem or network sandbox is unavailable"}
		}
		return nil
	})
}
func (p *Policy) CheckURL(u *url.URL) error {
	origin, err := originOf(u)
	if err != nil {
		return deny("network", "destination is not an HTTP(S) origin")
	}
	if u.User != nil {
		return deny("network", "credentials in URLs are not allowed")
	}
	return p.each(func(p *Policy) error {
		if p.all || p.config.NetworkUnrestricted || slices.Contains(p.config.NetworkOrigins, origin) {
			return nil
		}
		return deny("network", "destination is not allowed")
	})
}
func originOf(u *url.URL) (string, error) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", errors.New("invalid HTTP(S) origin")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

func within(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	return relative, err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// rootRelative preserves explicitly configured aliases (e.g. macOS /var) while
// all I/O remains relative to the pinned canonical root.
func rootRelative(r *root, path string) (string, bool) {
	if relative, ok := within(r.path, path); ok {
		return relative, true
	}
	return within(r.alias, path)
}
func canonicalPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

// CheckPath is preflight only. Executors must use rooted descriptor-relative IO
// (OpenFile/OpenParent, or their own os.Root) to prevent symlink-swap races.
func (p *Policy) CheckPath(path string, write bool) error {
	_, _, err := p.selectRoot(path, write)
	return err
}
func (p *Policy) unrestrictedFilesystem() bool {
	return p.each(func(p *Policy) error {
		if p.all || p.config.FilesystemUnrestricted {
			return nil
		}
		return deny("filesystem", "restricted")
	}) == nil
}
func (p *Policy) selectRoot(path string, write bool) (*root, string, error) {
	// Resolving ".." lexically before a symlink would change kernel semantics.
	// Reject ambiguous traversal instead of silently opening a different target.
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return nil, "", deny("filesystem", "parent traversal is not allowed")
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", deny("filesystem", "invalid path")
	}
	canonical, err := canonicalPath(absolute)
	if err != nil {
		return nil, "", deny("filesystem", "path cannot be resolved safely")
	}
	var selected *root
	err = p.each(func(p *Policy) error {
		if p.all || p.config.FilesystemUnrestricted {
			return nil
		}
		roots := p.reads
		if write {
			roots = p.writes
		}
		var match *root
		for i := range roots {
			r := &roots[i]
			_, lexical := rootRelative(r, absolute)
			_, resolved := within(r.path, canonical)
			if lexical && resolved && (match == nil || len(r.path) > len(match.path)) {
				match = r
			}
		}
		if match == nil {
			return deny("filesystem", "path is outside allowed roots")
		}
		if selected == nil || len(match.path) > len(selected.path) {
			selected = match
		}
		return nil
	})
	return selected, absolute, err
}

// OpenFile opens through a pinned allowed root, without check-then-open races.
// Write flags require write access; O_RDWR requires both read and write access.
func (p *Policy) OpenFile(path string, flag int, mode os.FileMode) (*os.File, error) {
	if p.unrestrictedFilesystem() {
		return os.OpenFile(path, flag, mode)
	}
	write := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	r, absolute, err := p.selectRoot(path, write)
	if err != nil {
		return nil, err
	}
	if flag&os.O_RDWR != 0 {
		readRoot, _, err := p.selectRoot(path, false)
		if err != nil {
			return nil, err
		}
		if readRoot != nil && (r == nil || len(readRoot.path) > len(r.path)) {
			r = readRoot
		}
	}
	if r == nil {
		return os.OpenFile(absolute, flag, mode)
	}
	relative, _ := rootRelative(r, absolute)
	file, err := r.handle.OpenFile(relative, flag, mode)
	if errors.Is(err, os.ErrPermission) {
		return nil, deny("filesystem", "rooted file access was denied")
	}
	return file, err
}

// OpenParent authorizes the target path, then opens its parent under the pinned
// write root. Callers must use only basename-relative non-symlink operations on
// the returned descriptor. It never grants access to the parent itself.
func (p *Policy) OpenParent(path string) (*os.File, error) {
	if p.unrestrictedFilesystem() {
		parent, _ := filepath.Split(path)
		if parent == "" {
			parent = "."
		}
		return os.Open(parent)
	}
	r, absolute, err := p.selectRoot(path, true)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(absolute)
	if r == nil {
		return os.Open(parent)
	}
	relative, ok := rootRelative(r, parent)
	if !ok {
		return nil, deny("filesystem", "cannot modify an allowed root itself")
	}
	return r.handle.Open(relative)
}

type contextKey struct{}

// WithPolicy attaches a policy without widening an existing one.
func WithPolicy(ctx context.Context, p *Policy) context.Context {
	if p == nil {
		p = DenyAll()
	}
	if previous, ok := ctx.Value(contextKey{}).(*Policy); ok {
		p = previous.Intersect(p)
	}
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext preserves the legacy low-level API behavior for unscoped contexts.
// New Hosts must attach an explicit policy; a nil explicit policy denies all.
func FromContext(ctx context.Context) *Policy {
	if p, ok := ctx.Value(contextKey{}).(*Policy); ok {
		return p
	}
	return Unrestricted()
}

// Configured reports whether the caller supplied a policy explicitly.
func Configured(ctx context.Context) bool { _, ok := ctx.Value(contextKey{}).(*Policy); return ok }
