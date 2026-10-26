package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"slices"
)

// ErrIndependenceUnproven is deliberately closed: paths, content and OS error
// text must not become recovery diagnostics. It grants no Permission.
var ErrIndependenceUnproven = errors.New("mutation independence unproven")

type independenceKey struct{}
type independenceRequirementKey struct{}
type blockedFootprintsKey struct{}

type independencePath struct {
	path     string
	parents  []os.FileInfo
	target   os.FileInfo
	revision Revision
	size     int64
}

// IndependenceFence is ephemeral execution evidence, never a durable grant.
// Its complete request digest binds all targets/Expected revisions/content;
// it does not retain a second copy of the mutation payload.
type IndependenceFence struct {
	service    *Service
	digest     [sha256.Size]byte
	candidates []independencePath
	blocked    []independencePath
	historical []Footprint
}

// WithIndependenceRequirement asks the actual executor to prove its complete
// prepared mutation request. In particular Edit's preparation is not duplicated
// by an outer caller. Empty/oversized requirements fail closed in apply.
func WithIndependenceRequirement(ctx context.Context, blocked []string) context.Context {
	return context.WithValue(ctx, independenceRequirementKey{}, append([]string(nil), blocked...))
}

func WithIndependenceFence(ctx context.Context, fence *IndependenceFence) context.Context {
	return context.WithValue(ctx, independenceKey{}, fence)
}

func independenceDigest(request Request) ([sha256.Size]byte, error) {
	hash := sha256.New()
	var size [8]byte
	put := func(data []byte) {
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		hash.Write(size[:])
		hash.Write(data)
	}
	put([]byte{byte(request.Version)})
	for _, change := range request.Changes {
		put([]byte(change.Path))
		if change.Expected.Exists {
			put([]byte{1})
		} else {
			put([]byte{0})
		}
		put([]byte(change.Expected.Digest))
		content := sha256.Sum256(change.Content)
		put(content[:])
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

// ProveIndependent is limited to finite native replacement targets. Callers
// must independently prove that the blocked executor has no additional effects
// or ordering/input dependencies. Bash/unknown executors do not qualify.
func (s *Service) ProveIndependent(ctx context.Context, request Request, blocked []string) (*IndependenceFence, error) {
	if s == nil || request.Validate() != nil {
		return nil, ErrIndependenceUnproven
	}
	paths := make([]string, len(request.Changes))
	for i, change := range request.Changes {
		paths[i] = change.Path
	}
	locks, err := s.lockIndependencePaths(ctx, paths)
	if err != nil {
		return nil, ErrIndependenceUnproven
	}
	defer closeIndependenceLocks(locks)
	return s.proveIndependent(ctx, request, blocked, true)
}

// Public observations participate in the same cooperating-writer locks as
// Snapshot/apply. Internal apply helpers already own all these target locks.
func (s *Service) lockIndependencePaths(ctx context.Context, paths []string) ([]*os.File, error) {
	if s == nil || len(paths) == 0 || len(paths) > MaxTargets {
		return nil, ErrIndependenceUnproven
	}
	ordered := make([]string, len(paths))
	for i, path := range paths {
		canonical, err := s.Path(path)
		if err != nil {
			return nil, ErrIndependenceUnproven
		}
		ordered[i] = canonical
	}
	slices.Sort(ordered)
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1] == ordered[i] {
			return nil, ErrIndependenceUnproven
		}
	}
	locks := make([]*os.File, 0, len(ordered))
	for _, path := range ordered {
		lock, err := s.lock(ctx, path)
		if err != nil {
			closeIndependenceLocks(locks)
			return nil, ErrIndependenceUnproven
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func closeIndependenceLocks(locks []*os.File) {
	for _, lock := range locks {
		lock.Close()
	}
}

func (s *Service) proveIndependent(ctx context.Context, request Request, blocked []string, readCandidates bool) (*IndependenceFence, error) {
	if s == nil || s.alias != s.root || request.Validate() != nil || len(blocked) == 0 || len(blocked) > MaxTargets || ctx.Err() != nil {
		return nil, ErrIndependenceUnproven
	}
	digest, err := independenceDigest(request)
	if err != nil {
		return nil, err
	}
	fence := &IndependenceFence{service: s, digest: digest}
	if historical, ok := ctx.Value(blockedFootprintsKey{}).([]Footprint); ok {
		fence.historical = historical
	}
	seen := map[string]bool{}
	var bytes int64
	for _, raw := range blocked {
		path, err := s.Path(raw)
		if err != nil || seen[path] {
			return nil, ErrIndependenceUnproven
		}
		seen[path] = true
		// A finite replacement's disjointness depends on identities and effect
		// locations, not its current contents. Never read the uncertain body.
		proof, err := s.captureIndependenceMetadata(ctx, path)
		if err != nil {
			return nil, ErrIndependenceUnproven
		}
		fence.blocked = append(fence.blocked, proof)
	}
	for _, change := range request.Changes {
		path, err := s.Path(change.Path)
		if err != nil || seen[path] {
			return nil, ErrIndependenceUnproven
		}
		seen[path] = true
		proof, err := s.captureAuthorizedIndependence(ctx, path, readCandidates, true)
		if err != nil || readCandidates && proof.revision != change.Expected {
			return nil, ErrIndependenceUnproven
		}
		if !readCandidates {
			// apply owns the locked Expected read immediately after metadata
			// preflight. Replacement revalidation still checks the actual bytes.
			proof.revision = change.Expected
		}
		bytes += proof.size
		if bytes > MaxFileBytes {
			return nil, ErrIndependenceUnproven
		}
		for _, old := range fence.blocked {
			if old.target != nil && proof.target != nil && os.SameFile(old.target, proof.target) {
				return nil, ErrIndependenceUnproven
			}
		}
		fence.candidates = append(fence.candidates, proof)
	}
	if fence.revalidate(ctx, 0, readCandidates) != nil {
		return nil, ErrIndependenceUnproven
	}
	return fence, nil
}

func sameIndependencePath(before, after independencePath, target bool) bool {
	if before.path != after.path || len(before.parents) != len(after.parents) {
		return false
	}
	for i := range before.parents {
		if !os.SameFile(before.parents[i], after.parents[i]) {
			return false
		}
	}
	if !target {
		return true // An earlier target in this exact batch was intentionally replaced.
	}
	if (before.target == nil) != (after.target == nil) || before.revision != after.revision {
		return false
	}
	return before.target == nil || os.SameFile(before.target, after.target)
}

func (f *IndependenceFence) Revalidate(ctx context.Context) error {
	if f == nil || f.service == nil {
		return ErrIndependenceUnproven
	}
	paths := make([]string, len(f.candidates))
	for i, candidate := range f.candidates {
		paths[i] = candidate.path
	}
	locks, err := f.service.lockIndependencePaths(ctx, paths)
	if err != nil {
		return ErrIndependenceUnproven
	}
	defer closeIndependenceLocks(locks)
	return f.revalidate(ctx, 0, true)
}

func (f *IndependenceFence) revalidate(ctx context.Context, applied int, readCandidates bool) error {
	if f == nil || f.service == nil || ctx.Err() != nil || len(f.candidates) == 0 || len(f.blocked) == 0 {
		return ErrIndependenceUnproven
	}
	for _, footprint := range f.historical {
		if f.service.ValidateFootprint(ctx, footprint) != nil {
			return ErrIndependenceUnproven
		}
	}
	for _, old := range f.blocked {
		now, err := f.service.captureIndependenceMetadata(ctx, old.path)
		if err != nil || !sameIndependencePath(old, now, true) {
			return ErrIndependenceUnproven
		}
	}
	for i, old := range f.candidates {
		read := readCandidates && i >= applied
		now, err := f.service.captureAuthorizedIndependence(ctx, old.path, read, true)
		if !read {
			now.revision = old.revision
		}
		if err != nil || !sameIndependencePath(old, now, i >= applied) {
			return ErrIndependenceUnproven
		}
		if now.target != nil {
			identity, ok := footprintIdentity(now.target)
			if !ok {
				return ErrIndependenceUnproven
			}
			for _, historical := range f.historical {
				for _, target := range historical.Targets {
					if target.Target != nil && *target.Target == identity {
						return ErrIndependenceUnproven
					}
				}
			}
		}
	}
	return nil
}

func (s *Service) prepareIndependence(ctx context.Context, request Request) (context.Context, error) {
	if paths, present := ctx.Value(independenceRequirementKey{}).([]string); present {
		fence, err := s.proveIndependent(ctx, request, paths, false)
		if err != nil {
			return ctx, err
		}
		return WithIndependenceFence(ctx, fence), nil
	}
	return ctx, nil
}

func (s *Service) checkIndependence(ctx context.Context, request Request, applied int) error {
	return s.checkIndependenceWithRead(ctx, request, applied, true)
}

func (s *Service) checkIndependenceMetadata(ctx context.Context, request Request, applied int) error {
	return s.checkIndependenceWithRead(ctx, request, applied, false)
}

func (s *Service) checkIndependenceWithRead(ctx context.Context, request Request, applied int, readCandidates bool) error {
	value := ctx.Value(independenceKey{})
	if value == nil {
		return nil
	}
	fence, ok := value.(*IndependenceFence)
	if !ok || fence == nil || fence.service != s {
		return ErrIndependenceUnproven
	}
	digest, err := independenceDigest(request)
	if err != nil || digest != fence.digest {
		return ErrIndependenceUnproven
	}
	return fence.revalidate(ctx, applied, readCandidates)
}

func (s *Service) checkIndependenceParent(ctx context.Context, index int, parent *os.File) error {
	value := ctx.Value(independenceKey{})
	if value == nil {
		return nil
	}
	fence, ok := value.(*IndependenceFence)
	if !ok || fence == nil || fence.service != s || index < 0 || index >= len(fence.candidates) || parent == nil {
		return ErrIndependenceUnproven
	}
	parents := fence.candidates[index].parents
	actual, err := parent.Stat()
	if err != nil || len(parents) == 0 || !os.SameFile(parents[len(parents)-1], actual) {
		return ErrIndependenceUnproven
	}
	return nil
}
