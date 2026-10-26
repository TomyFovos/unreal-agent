//go:build linux || darwin

package mutation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func independenceWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func independenceContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatal("synthetic file changed unexpectedly")
	}
}

func TestIndependenceSameDirectoryWriteAndEdit(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "same/a"), "uncertain")
	blocked, err := s.CaptureFootprint(t.Context(), []string{"same/a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBlockedFootprints(WithIndependenceRequirement(t.Context(), []string{"same/a"}), []Footprint{blocked})
	one := s.Execute(ctx, "synthetic-independent-write", request("same/b", Revision{}, "before"))
	if one.Code != Applied {
		t.Fatal(one)
	}
	two := s.Execute(ctx, "synthetic-independent-edit", request("same/b", RevisionOf([]byte("before")), "after"))
	if two.Code != Applied {
		t.Fatal(two)
	}
	independenceContents(t, filepath.Join(s.root, "same/b"), "after")
	independenceContents(t, filepath.Join(s.root, "same/a"), "uncertain")
	// Existing stable Mutation receipts remain the execution evidence.
	if recovered, exists, err := s.Recover(t.Context(), "synthetic-independent-edit"); err != nil || !exists || recovered.Code != Applied || recovered.Targets[0].Revision != RevisionOf([]byte("after")) {
		t.Fatal("missing canonical mutation receipt")
	}
}

func TestIndependenceRejectsWholeOverlappingBatch(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "a"), "uncertain")
	for _, alias := range []string{"a", "./a", filepath.Join(s.root, "a")} {
		r := Request{Version: Version, Changes: []Change{{Path: "b", Content: []byte("must-not-write")}, {Path: alias, Expected: RevisionOf([]byte("uncertain")), Content: []byte("must-not-change")}}}
		if got := s.Apply(WithIndependenceRequirement(t.Context(), []string{"a"}), r); got.Code != Invalid {
			t.Fatal("overlapping batch admitted")
		}
	}
	if _, err := os.Stat(filepath.Join(s.root, "b")); !os.IsNotExist(err) {
		t.Fatal("partial batch side effect")
	}
	independenceContents(t, filepath.Join(s.root, "a"), "uncertain")
}

func TestIndependenceRejectsAliasesAndMissingProof(t *testing.T) {
	for _, kind := range []string{"candidate_symlink", "blocked_symlink", "hardlink", "parent_symlink", "root_alias", "empty", "too_many", "stale"} {
		t.Run(kind, func(t *testing.T) {
			s := service(t)
			independenceWrite(t, filepath.Join(s.root, "a"), "a")
			independenceWrite(t, filepath.Join(s.root, "b"), "b")
			candidate, blocked := request("b", RevisionOf([]byte("b")), "new"), []string{"a"}
			switch kind {
			case "candidate_symlink":
				os.Remove(filepath.Join(s.root, "b"))
				if err := os.Symlink("a", filepath.Join(s.root, "b")); err != nil {
					t.Fatal(err)
				}
			case "blocked_symlink":
				os.Remove(filepath.Join(s.root, "a"))
				if err := os.Symlink("b", filepath.Join(s.root, "a")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				os.Remove(filepath.Join(s.root, "b"))
				if err := os.Link(filepath.Join(s.root, "a"), filepath.Join(s.root, "b")); err != nil {
					t.Fatal(err)
				}
			case "parent_symlink":
				if err := os.Symlink(".", filepath.Join(s.root, "alias")); err != nil {
					t.Fatal(err)
				}
				candidate.Changes[0].Path = "alias/b"
			case "root_alias":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(s.root, alias); err != nil {
					t.Fatal(err)
				}
				var err error
				s, err = New(Config{Root: alias, StateDir: s.state, Authorize: func(context.Context, string, bool) error { return nil }})
				if err != nil {
					t.Fatal(err)
				}
			case "empty":
				blocked = nil
			case "too_many":
				blocked = make([]string, MaxTargets+1)
			case "stale":
				candidate.Changes[0].Expected = RevisionOf([]byte("old"))
			}
			if _, err := s.ProveIndependent(t.Context(), candidate, blocked); !errors.Is(err, ErrIndependenceUnproven) {
				t.Fatal("unsafe proof accepted")
			}
		})
	}
}

func TestIndependenceFenceDetectsExecutionBoundaryReplacement(t *testing.T) {
	for _, fault := range []string{"candidate_inode", "blocked_inode", "candidate_parent", "blocked_parent", "root", "symlink", "hardlink", "revision"} {
		t.Run(fault, func(t *testing.T) {
			s := service(t)
			a, b := filepath.Join(s.root, "one/a"), filepath.Join(s.root, "two/b")
			independenceWrite(t, a, "a")
			independenceWrite(t, b, "b")
			r := request("two/b", RevisionOf([]byte("b")), "new")
			fence, err := s.ProveIndependent(t.Context(), r, []string{"one/a"})
			if err != nil {
				t.Fatal(err)
			}
			// The fault occurs after apply's lock/preflight checks, at the last
			// existing hook before the descriptor-relative replacement.
			triggered := false
			s.hook = func(stage string, index int) error {
				if stage != "before_replace" || index != 0 {
					return nil
				}
				triggered = true
				switch fault {
				case "candidate_inode":
					if err := os.Rename(b, b+"-old"); err != nil {
						return err
					}
					independenceWrite(t, b, "b")
				case "blocked_inode":
					if err := os.Rename(a, a+"-old"); err != nil {
						return err
					}
					independenceWrite(t, a, "a")
				case "candidate_parent":
					if err := os.Rename(filepath.Dir(b), filepath.Dir(b)+"-old"); err != nil {
						return err
					}
					independenceWrite(t, b, "b")
				case "blocked_parent":
					if err := os.Rename(filepath.Dir(a), filepath.Dir(a)+"-old"); err != nil {
						return err
					}
					independenceWrite(t, a, "a")
				case "root":
					if err := os.Rename(s.root, s.root+"-old"); err != nil {
						return err
					}
					independenceWrite(t, a, "a")
					independenceWrite(t, b, "b")
				case "symlink":
					if err := os.Remove(b); err != nil {
						return err
					}
					return os.Symlink("../one/a", b)
				case "hardlink":
					return os.Link(b, b+"-alias")
				case "revision":
					independenceWrite(t, b, "external")
				}
				return nil
			}
			got := s.Apply(WithIndependenceFence(t.Context(), fence), r)
			if !triggered || got.Code != Invalid || got.Targets[0].Code == Applied {
				t.Fatal("fault did not fail closed at replacement boundary", got.Code)
			}
			if fault == "candidate_parent" {
				independenceContents(t, filepath.Join(filepath.Dir(b)+"-old", "b"), "b")
			}
			if fault == "root" {
				independenceContents(t, filepath.Join(s.root+"-old", "two/b"), "b")
			}
		})
	}
}

func TestIndependenceHistoricalParentIdentityCannotBeRebased(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "old/a"), "uncertain")
	old, err := s.CaptureFootprint(t.Context(), []string{"old/a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.root, "old"), filepath.Join(s.root, "moved")); err != nil {
		t.Fatal(err)
	}
	independenceWrite(t, filepath.Join(s.root, "old/a"), "decoy")
	// Today's names are disjoint, but the original A effect may now be at
	// moved/a. A fresh pathname-only proof must not erase that restriction.
	r := request("moved/b", Revision{}, "must-not-write")
	ctx := WithBlockedFootprints(WithIndependenceRequirement(t.Context(), []string{"old/a"}), []Footprint{old})
	if s.ValidateFootprint(t.Context(), old) == nil {
		t.Fatal("historical parent replacement accepted")
	}
	if got := s.Apply(ctx, r); got.Code != Invalid {
		t.Fatal("historical footprint silently rebased")
	}
	if _, err := os.Stat(filepath.Join(s.root, "moved/b")); !os.IsNotExist(err) {
		t.Fatal("replacement parent permitted a side effect")
	}
}

func TestIndependenceHistoricalTargetRenameCannotBecomeCandidate(t *testing.T) {
	s := service(t)
	a, b := filepath.Join(s.root, "a"), filepath.Join(s.root, "b")
	independenceWrite(t, a, "uncertain")
	old, err := s.CaptureFootprint(t.Context(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	independenceWrite(t, a, "decoy")
	ctx := WithBlockedFootprints(WithIndependenceRequirement(t.Context(), []string{"a"}), []Footprint{old})
	if got := s.Apply(ctx, request("b", RevisionOf([]byte("uncertain")), "must-not-write")); got.Code != Invalid {
		t.Fatal("historical target inode was retargeted through a new pathname")
	}
	independenceContents(t, b, "uncertain")
}

func TestIndependenceProofDoesNotGrantPermission(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "a"), "a")
	f, err := s.ProveIndependent(t.Context(), request("b", Revision{}, "b"), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	s.authorize = func(context.Context, string, bool) error { return errors.New("synthetic denial") }
	if got := s.Apply(WithIndependenceFence(t.Context(), f), request("b", Revision{}, "b")); got.Code != Denied {
		t.Fatal("proof bypassed normal mutation authorization")
	}
	if _, err := os.Stat(filepath.Join(s.root, "b")); !os.IsNotExist(err) {
		t.Fatal("denied write executed")
	}
}

func TestIndependenceBindsPayloadServiceAndCancellation(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "a"), "a")
	r := request("b", Revision{}, "b")
	f, err := s.ProveIndependent(t.Context(), r, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	changed := request("b", Revision{}, "different")
	if got := s.Apply(WithIndependenceFence(t.Context(), f), changed); got.Code != Invalid {
		t.Fatal("fence payload binding lost")
	}
	other := service(t)
	if got := other.Apply(WithIndependenceFence(t.Context(), f), r); got.Code != Invalid {
		t.Fatal("cross-service fence accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ProveIndependent(ctx, r, []string{"a"}); err == nil {
		t.Fatal("canceled proof accepted")
	}
	if _, err := os.Stat(filepath.Join(s.root, "b")); !os.IsNotExist(err) {
		t.Fatal("rejected mutation was applied")
	}
}

func TestIndependenceFullMultiTargetPreparedRequest(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "a"), "a")
	r := Request{Version: Version, Changes: []Change{{Path: "b", Content: []byte("b")}, {Path: "c", Content: []byte("c")}}}
	ctx := WithIndependenceRequirement(t.Context(), []string{"a"})
	got := s.ExecutePrepared(ctx, "synthetic-prepared", []byte("synthetic-edit-identity"), func(context.Context) (Request, error) { return r, nil })
	if got.Code != Applied || len(got.Targets) != 2 || got.Targets[0].Code != Applied || got.Targets[1].Code != Applied {
		t.Fatal(got)
	}
	independenceContents(t, filepath.Join(s.root, "b"), "b")
	independenceContents(t, filepath.Join(s.root, "c"), "c")
}

func TestIndependenceRejectsTemporarilyRetargetedExecutionDescriptor(t *testing.T) {
	s := service(t)
	a, b := filepath.Join(s.root, "one/a"), filepath.Join(s.root, "two/b")
	independenceWrite(t, a, "a")
	independenceWrite(t, b, "b")
	rogue := filepath.Join(s.root, "rogue")
	independenceWrite(t, filepath.Join(rogue, "b"), "b")
	r := request("two/b", RevisionOf([]byte("b")), "must-not-write")
	fence, err := s.ProveIndependent(t.Context(), r, []string{"one/a"})
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(b)
	saved := parent + "-saved"
	swapped, restored := false, false
	s.authorize = func(_ context.Context, _ string, write bool) error {
		if !write || swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(parent, saved); err != nil {
			return err
		}
		return os.Rename(rogue, parent)
	}
	s.hook = func(stage string, _ int) error {
		// Restore the pathname after pinning the rogue execution descriptor.
		// The final parent-FD guard, rather than a fresh pathname, must reject it.
		if stage != "after_parent" {
			return nil
		}
		if err := os.Rename(parent, rogue); err != nil {
			return err
		}
		if err := os.Rename(saved, parent); err != nil {
			return err
		}
		restored = true
		return nil
	}
	got := s.Apply(WithIndependenceFence(t.Context(), fence), r)
	if !swapped || !restored {
		t.Fatal("fault did not reach pinned execution descriptor")
	}
	if got.Code != Invalid {
		t.Fatal("different pinned parent descriptor passed path-only proof", got.Code)
	}
	independenceContents(t, b, "b")
	independenceContents(t, filepath.Join(rogue, "b"), "b")
}
