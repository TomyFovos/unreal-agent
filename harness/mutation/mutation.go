// Package mutation provides conditional replacement for cooperating local writers.
// It is not filesystem-wide CAS: external writers, mounts and directory renames
// require separate workspace isolation.
package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const Version = 1
const MaxFileBytes = 16 << 20
const MaxTargets = 128

type Code string

const (
	Applied       Code = "applied"
	Stale         Code = "stale"
	Invalid       Code = "invalid"
	Denied        Code = "permission_denied"
	Failed        Code = "failed"
	Canceled      Code = "canceled"
	Indeterminate Code = "indeterminate"
	NotApplied    Code = "not_applied"
)

type Revision struct {
	Exists bool   `json:"exists"`
	Digest string `json:"digest,omitempty"`
}
type Snapshot struct {
	Path     string   `json:"path"`
	Revision Revision `json:"revision"`
	Data     []byte   `json:"-"`
}

func RevisionOf(data []byte) Revision {
	digest := sha256.Sum256(data)
	return Revision{Exists: true, Digest: hex.EncodeToString(digest[:])}
}
func (r Revision) Validate() error {
	if !r.Exists {
		if r.Digest != "" {
			return errors.New("absent revision cannot carry a digest")
		}
		return nil
	}
	d, err := hex.DecodeString(r.Digest)
	if err != nil || len(d) != sha256.Size || r.Digest != strings.ToLower(r.Digest) {
		return errors.New("existing revision requires lowercase SHA-256 digest")
	}
	return nil
}

type Change struct {
	Path     string   `json:"path"`
	Expected Revision `json:"expected"`
	Content  []byte   `json:"content"`
}
type Request struct {
	Version int      `json:"version"`
	Changes []Change `json:"changes"`
}

func (r Request) Validate() error {
	if r.Version != Version {
		return errors.New("unsupported mutation version")
	}
	if len(r.Changes) == 0 || len(r.Changes) > MaxTargets {
		return errors.New("mutation requires 1 to 128 targets")
	}
	total := 0
	for _, c := range r.Changes {
		if c.Path == "" || len(c.Path) > 4096 || strings.IndexByte(c.Path, 0) >= 0 {
			return errors.New("invalid path")
		}
		if err := c.Expected.Validate(); err != nil {
			return err
		}
		total += len(c.Content)
		if len(c.Content) > MaxFileBytes || total > MaxFileBytes {
			return errors.New("mutation exceeds 16 MiB payload limit")
		}
	}
	return nil
}

type TargetResult struct {
	Path     string   `json:"path"`
	Code     Code     `json:"code"`
	Revision Revision `json:"revision"`
}
type Result struct {
	Version int            `json:"version"`
	Code    Code           `json:"code"`
	Targets []TargetResult `json:"targets"`
}
type Authorizer func(context.Context, string, bool) error
type Config struct {
	Root string
	// StateDir must be private, durable local storage shared by all sessions and
	// processes for this workspace. It must not be deleted while operations exist.
	StateDir  string
	Authorize Authorizer
}
type Service struct {
	root, alias, state string
	authorize          Authorizer
	hook               func(string, int) error
}

func New(config Config) (*Service, error) {
	if config.Authorize == nil {
		return nil, errors.New("mutation requires an execution authorizer")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, err
	}
	alias := root
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, errors.New("workspace root must be a directory")
	}
	state := config.StateDir
	if state == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(root))
		state = filepath.Join(cache, "unreal-agent", "mutation", hex.EncodeToString(hash[:]))
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	info, err = os.Lstat(state)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("mutation state must be a private directory")
	}
	state, err = filepath.EvalSymlinks(state)
	if err != nil {
		return nil, err
	}
	return &Service{root: root, alias: alias, state: state, authorize: config.Authorize}, nil
}
func (s *Service) Root() string { return s.root }
func (s *Service) Path(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("invalid path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	path = filepath.Clean(path)
	// Translate only the root alias explicitly supplied to New. Descendant
	// symlinks are still rejected by the descriptor-relative filesystem layer.
	if s.alias != s.root {
		rel, err := filepath.Rel(s.alias, path)
		if err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			path = filepath.Join(s.root, rel)
		}
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path outside workspace")
	}
	stateRel, err := filepath.Rel(s.state, path)
	if err == nil && stateRel != ".." && !strings.HasPrefix(stateRel, ".."+string(filepath.Separator)) {
		return "", errors.New("mutation state is not a tool target")
	}
	return path, nil
}
func (s *Service) Snapshot(ctx context.Context, path string) (Snapshot, error) {
	canonical, err := s.Path(path)
	if err != nil {
		return Snapshot{}, err
	}
	if err = s.authorize(ctx, canonical, false); err != nil {
		return Snapshot{}, err
	}
	if err = ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	// Serialize observation with cooperating replacements. Otherwise a file
	// opened just before rename may have link count zero by the time readTarget
	// validates its descriptor and be mistaken for an invalid target.
	lock, err := s.lock(ctx, canonical)
	if err != nil {
		return Snapshot{}, err
	}
	defer lock.Close()
	parent, base, err := s.openParent(canonical)
	if err != nil {
		return Snapshot{}, err
	}
	defer parent.Close()
	data, exists, _, err := readTarget(parent, base)
	if err != nil {
		return Snapshot{}, err
	}
	revision := Revision{}
	if exists {
		revision = RevisionOf(data)
	}
	return Snapshot{Path: canonical, Revision: revision, Data: data}, nil
}
func (s *Service) lock(ctx context.Context, key string) (*os.File, error) {
	sum := sha256.Sum256([]byte(s.root + "\x00" + key))
	return acquireLock(ctx, filepath.Join(s.state, hex.EncodeToString(sum[:])+".lock"))
}
func baseResult(request Request, code Code) Result {
	r := Result{Version: Version, Code: code, Targets: make([]TargetResult, len(request.Changes))}
	for i, c := range request.Changes {
		r.Targets[i] = TargetResult{Path: c.Path, Code: NotApplied}
	}
	return r
}

// Apply executes a fresh call. Durable operations must use Execute with a stable
// session+operation identity, so a lost result cannot cause an unknown replay.
func (s *Service) Apply(ctx context.Context, request Request) Result { return s.apply(ctx, request) }

type prepared struct {
	parent *os.File
	base   string
	mode   os.FileMode
}

func (s *Service) apply(ctx context.Context, request Request) Result {
	result := baseResult(request, Invalid)
	if request.Validate() != nil {
		return result
	}
	paths := make([]string, len(request.Changes))
	for i, c := range request.Changes {
		path, err := s.Path(c.Path)
		if err != nil {
			result.Targets[i].Code = Invalid
			return result
		}
		paths[i] = path
	}
	order := make([]int, len(paths))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return strings.Compare(paths[a], paths[b]) })
	for i := 1; i < len(order); i++ {
		if paths[order[i]] == paths[order[i-1]] {
			return result
		}
	}
	locks := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range locks {
			f.Close()
		}
	}()
	for _, i := range order {
		lock, err := s.lock(ctx, paths[i])
		if err != nil {
			result.Code = Failed
			if ctx.Err() != nil {
				result.Code = Canceled
			}
			return result
		}
		locks = append(locks, lock)
	}
	targets := make([]prepared, len(paths))
	defer func() {
		for _, p := range targets {
			if p.parent != nil {
				p.parent.Close()
			}
		}
	}()
	// Every target is opened and validated before creating any replacement.
	for i, path := range paths {
		if err := ctx.Err(); err != nil {
			result.Code = Canceled
			return result
		}
		if err := s.authorize(ctx, path, true); err != nil {
			result.Code = Denied
			result.Targets[i].Code = Denied
			return result
		}
		parent, base, err := s.openParent(path)
		if err != nil {
			result.Code = Invalid
			result.Targets[i].Code = Invalid
			return result
		}
		targets[i] = prepared{parent: parent, base: base, mode: 0600}
		data, exists, mode, err := readTarget(parent, base)
		if err != nil {
			result.Code = Invalid
			result.Targets[i].Code = Invalid
			return result
		}
		revision := Revision{}
		if exists {
			revision = RevisionOf(data)
			targets[i].mode = mode
		}
		result.Targets[i].Revision = revision
		if revision != request.Changes[i].Expected {
			result.Code = Stale
			result.Targets[i].Code = Stale
			return result
		}
	}
	result.Code = Applied
	for i, p := range targets {
		if ctx.Err() != nil {
			result.Code = Canceled
			result.Targets[i].Code = Canceled
			return result
		}
		if s.hook != nil {
			if err := s.hook("before_replace", i); err != nil {
				result.Code = Failed
				result.Targets[i].Code = Failed
				return result
			}
		}
		replaced, err := replaceTarget(p.parent, p.base, request.Changes[i].Content, p.mode)
		if err != nil {
			result.Code = Failed
			result.Targets[i].Code = Failed
			if replaced {
				result.Code = Indeterminate
				result.Targets[i].Code = Indeterminate
			}
			return result
		}
		result.Targets[i].Code = Applied
		result.Targets[i].Revision = RevisionOf(request.Changes[i].Content)
		if s.hook != nil {
			if err := s.hook("after_replace", i); err != nil {
				result.Code = Indeterminate
				result.Targets[i].Code = Indeterminate
				return result
			}
		}
	}
	return result
}

type receipt struct {
	Version int     `json:"version"`
	Digest  string  `json:"digest"`
	Result  *Result `json:"result,omitempty"`
}

// Execute durably records intent before any filesystem side effect. Existing
// unfinished intents fail indeterminate. Completed receipts are outcome evidence,
// not a second scheduler or session history.
func (s *Service) Execute(ctx context.Context, id string, request Request) Result {
	result := baseResult(request, Invalid)
	if id == "" || request.Validate() != nil {
		return result
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return result
	}
	digest := sha256.Sum256(encoded)
	requestHash := hex.EncodeToString(digest[:])
	key := sha256.Sum256([]byte(id))
	name := hex.EncodeToString(key[:]) + ".receipt"
	lock, err := s.lock(ctx, "receipt:"+id)
	if err != nil {
		result.Code = Failed
		if ctx.Err() != nil {
			result.Code = Canceled
		}
		return result
	}
	defer lock.Close()
	path := filepath.Join(s.state, name)
	existing, err := os.ReadFile(path)
	if err == nil {
		var saved receipt
		if json.Unmarshal(existing, &saved) != nil || saved.Version != Version || saved.Digest != requestHash {
			result.Code = Indeterminate
			return result
		}
		if saved.Result != nil {
			return *saved.Result
		}
		result.Code = Indeterminate
		for i := range result.Targets {
			result.Targets[i].Code = Indeterminate
		}
		return result
	}
	if !errors.Is(err, os.ErrNotExist) {
		result.Code = Failed
		return result
	}
	intent, _ := json.Marshal(receipt{Version: Version, Digest: requestHash})
	parent, err := os.Open(s.state)
	if err != nil {
		result.Code = Failed
		return result
	}
	defer parent.Close()
	if _, err = replaceTarget(parent, name, intent, 0600); err != nil {
		result.Code = Failed
		return result
	}
	if s.hook != nil {
		if s.hook("after_intent", -1) != nil {
			result.Code = Indeterminate
			return result
		}
	}
	result = s.apply(ctx, request)
	if s.hook != nil {
		if s.hook("before_receipt", -1) != nil {
			result.Code = Indeterminate
			return result
		}
	}
	completed, _ := json.Marshal(receipt{Version: Version, Digest: requestHash, Result: &result})
	if _, err = replaceTarget(parent, name, completed, 0600); err != nil {
		result.Code = Indeterminate
	}
	return result
}

// ReadBounded reads a complete snapshot or refuses oversized/nonregular files.
// A range consumer derives its revision from these exact bytes.
func ReadBounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", MaxFileBytes)
	}
	return data, nil
}
