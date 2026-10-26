package mutation

import (
	"context"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

type executionFootprintKey struct{}

// WithExecutionFootprint binds an admitted finite executor plan to the actual
// Mutation request and pinned directory descriptor. It grants no Permission.
func WithExecutionFootprint(ctx context.Context, footprint Footprint) context.Context {
	copy := cloneFootprint(footprint)
	return context.WithValue(ctx, executionFootprintKey{}, copy)
}

func cloneFootprint(f Footprint) Footprint {
	copy := Footprint{Version: f.Version, Root: f.Root, Targets: make([]FootprintTarget, len(f.Targets))}
	for i, target := range f.Targets {
		copy.Targets[i] = FootprintTarget{Path: target.Path, Parents: append([]FileIdentity(nil), target.Parents...)}
		if target.Target != nil {
			id := *target.Target
			copy.Targets[i].Target = &id
		}
	}
	return copy
}

// FileIdentity describes an observed inode, not an authority to open it.
type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type FootprintTarget struct {
	Path    string         `json:"path"`
	Parents []FileIdentity `json:"parents"`
	Target  *FileIdentity  `json:"target,omitempty"`
}

// Footprint is captured with current mutation authority before canonical
// admission, then bound by the Host to the exact typed plan. Recovery validates
// original parent identities rather than assuming today's pathname still names
// the original effect location.
type Footprint struct {
	Version int               `json:"version"`
	Root    string            `json:"root"`
	Targets []FootprintTarget `json:"targets"`
}

func (s *Service) CaptureFootprint(ctx context.Context, paths []string) (Footprint, error) {
	f := Footprint{Version: 1}
	if s == nil || s.alias != s.root || len(paths) == 0 || len(paths) > MaxTargets {
		return Footprint{}, ErrIndependenceUnproven
	}
	f.Root = s.root
	seen := map[string]bool{}
	for _, path := range paths {
		path, err := s.Path(path)
		if err != nil || seen[path] {
			return Footprint{}, ErrIndependenceUnproven
		}
		seen[path] = true
		p, err := s.captureAuthorizedIndependence(ctx, path, false, true)
		if err != nil {
			return Footprint{}, executionFootprintError(err)
		}
		target := FootprintTarget{Path: path}
		for _, info := range p.parents {
			id, ok := footprintIdentity(info)
			if !ok {
				return Footprint{}, ErrIndependenceUnproven
			}
			target.Parents = append(target.Parents, id)
		}
		if p.target != nil {
			id, ok := footprintIdentity(p.target)
			if !ok {
				return Footprint{}, ErrIndependenceUnproven
			}
			target.Target = &id
		}
		f.Targets = append(f.Targets, target)
	}
	return f, nil
}

func (s *Service) ValidateFootprint(ctx context.Context, f Footprint) error {
	return s.validateFootprint(ctx, f, false)
}

// ValidateExecutionFootprint checks the current mutation's own effect bounds
// under its write authority. Historical targets use ValidateFootprint instead;
// their old mutation grants never authorize a new inspection.
func (s *Service) ValidateExecutionFootprint(ctx context.Context, f Footprint) error {
	return s.validateFootprint(ctx, f, true)
}

func executionFootprintError(err error) error {
	if denied := permission.Failure(err); denied != nil {
		return denied
	}
	return ErrIndependenceUnproven
}

func (s *Service) validateFootprint(ctx context.Context, f Footprint, write bool) error {
	if s == nil || s.alias != s.root || f.Version != 1 || f.Root != s.root || len(f.Targets) == 0 || len(f.Targets) > MaxTargets {
		return ErrIndependenceUnproven
	}
	seen := map[string]bool{}
	for _, old := range f.Targets {
		path, err := s.Path(old.Path)
		if err != nil || path != old.Path || seen[path] || len(old.Parents) == 0 || len(old.Parents) > 128 {
			return ErrIndependenceUnproven
		}
		seen[path] = true
		now, err := s.captureAuthorizedIndependence(ctx, path, false, write)
		if err != nil {
			if write {
				return executionFootprintError(err)
			}
			return ErrIndependenceUnproven
		}
		if len(now.parents) != len(old.Parents) {
			return ErrIndependenceUnproven
		}
		for i, info := range now.parents {
			id, ok := footprintIdentity(info)
			if !ok || id.Inode == 0 || id != old.Parents[i] {
				return ErrIndependenceUnproven
			}
		}
	}
	return nil
}

// WithBlockedFootprints adds immutable historical parent evidence to every
// execution-time fence check. Host validates plan/path coverage separately.
func WithBlockedFootprints(ctx context.Context, footprints []Footprint) context.Context {
	copied := make([]Footprint, len(footprints))
	for i, f := range footprints {
		copied[i] = cloneFootprint(f)
	}
	return context.WithValue(ctx, blockedFootprintsKey{}, copied)
}

// checkExecutionFootprint validates the actual executor request, not an outer
// tool's declaration. At replacement it also checks the exact descriptor used
// for Renameat; reopening a restored pathname cannot bless another pinned FD.
// All inspection here is metadata-only. Existing revision reads remain owned
// by apply's normal target locks and expected-revision validation.
func (s *Service) checkExecutionFootprint(ctx context.Context, request Request, index int, parent *os.File) error {
	value := ctx.Value(executionFootprintKey{})
	if value == nil {
		return nil
	}
	footprint, ok := value.(Footprint)
	if !ok || len(request.Changes) != len(footprint.Targets) || s.ValidateExecutionFootprint(ctx, footprint) != nil {
		return ErrIndependenceUnproven
	}
	targets := make(map[string]FootprintTarget, len(footprint.Targets))
	for _, target := range footprint.Targets {
		targets[target.Path] = target
	}
	for _, change := range request.Changes {
		path, err := s.Path(change.Path)
		if err != nil {
			return ErrIndependenceUnproven
		}
		if _, exists := targets[path]; !exists {
			return ErrIndependenceUnproven
		}
		delete(targets, path)
	}
	if len(targets) != 0 {
		return ErrIndependenceUnproven
	}
	if index < 0 {
		return nil
	}
	if index >= len(request.Changes) || parent == nil {
		return ErrIndependenceUnproven
	}
	path, err := s.Path(request.Changes[index].Path)
	if err != nil {
		return ErrIndependenceUnproven
	}
	actual, err := parent.Stat()
	if err != nil {
		return ErrIndependenceUnproven
	}
	identity, ok := footprintIdentity(actual)
	if !ok {
		return ErrIndependenceUnproven
	}
	for _, target := range footprint.Targets {
		if target.Path == path && len(target.Parents) > 0 && target.Parents[len(target.Parents)-1] == identity {
			return nil
		}
	}
	return ErrIndependenceUnproven
}
