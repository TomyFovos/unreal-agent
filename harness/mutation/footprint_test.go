//go:build linux || darwin

package mutation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionFootprintRejectsParentRetargetBeforeFirstExecution(t *testing.T) {
	s := service(t)
	parent := filepath.Join(s.root, "parent")
	rogue := filepath.Join(s.root, "rogue")
	independenceWrite(t, filepath.Join(parent, "a"), "original")
	independenceWrite(t, filepath.Join(rogue, "a"), "original")
	basis, err := s.CaptureFootprint(t.Context(), []string{"parent/a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionFootprint(t.Context(), basis)
	// No blocked effect, no IndependenceRequirement and no recovery Fence:
	// this is the initial operation whose later uncertainty needs true bounds.
	if s.ValidateFootprint(ctx, basis) != nil {
		t.Fatal("preflight footprint must initially validate")
	}
	saved := parent + "-saved"
	swapped, restored := false, false
	s.authorize = func(_ context.Context, _ string, write bool) error {
		if !write || swapped {
			return nil
		}
		if err := os.Rename(parent, saved); err != nil {
			return err
		}
		if err := os.Rename(rogue, parent); err != nil {
			return err
		}
		swapped = true
		return nil
	}
	s.hook = func(stage string, _ int) error {
		// Restore the pathname immediately after the execution descriptor was
		// pinned, so metadata preflight cannot detect the transient swap. The
		// final guard must compare that actual descriptor with the footprint.
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
	got := s.Apply(ctx, request("parent/a", RevisionOf([]byte("original")), "must-not-write"))
	if !swapped || !restored {
		t.Fatal("negative fault did not reach the actual pinned parent descriptor")
	}
	if got.Code != Invalid {
		t.Fatal("first execution used a different parent than its admitted footprint", got.Code)
	}
	independenceContents(t, filepath.Join(parent, "a"), "original")
	independenceContents(t, filepath.Join(rogue, "a"), "original")
}

func TestExecutionFootprintRequiresCompleteActualTargetSet(t *testing.T) {
	for _, mode := range []string{"normal_multi", "incomplete", "extra", "different", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			s := service(t)
			basis, err := s.CaptureFootprint(t.Context(), []string{"c", "b"})
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithExecutionFootprint(t.Context(), basis)
			r := Request{Version: Version, Changes: []Change{{Path: "b", Content: []byte("B")}, {Path: "c", Content: []byte("C")}}}
			switch mode {
			case "incomplete":
				r.Changes = r.Changes[:1]
			case "extra":
				r.Changes = append(r.Changes, Change{Path: "d", Content: []byte("D")})
			case "different":
				r.Changes[1].Path = "d"
			case "duplicate":
				r.Changes[1].Path = "./b"
			}
			got := s.Apply(ctx, r)
			if mode == "normal_multi" {
				if got.Code != Applied || len(got.Targets) != 2 || got.Targets[0].Code != Applied || got.Targets[1].Code != Applied {
					t.Fatal("complete multi-target footprint rejected", got.Code)
				}
				independenceContents(t, filepath.Join(s.root, "b"), "B")
				independenceContents(t, filepath.Join(s.root, "c"), "C")
			} else {
				if got.Code != Invalid {
					t.Fatal("incomplete or changed target set accepted", got.Code)
				}
				for _, path := range []string{"b", "c", "d"} {
					if _, err := os.Stat(filepath.Join(s.root, path)); !os.IsNotExist(err) {
						t.Fatal("invalid target set partially executed")
					}
				}
			}
		})
	}
}

func TestExecutionFootprintContextDoesNotAliasCallerMetadata(t *testing.T) {
	s := service(t)
	basis, err := s.CaptureFootprint(t.Context(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionFootprint(t.Context(), basis)
	basis.Targets[0].Path = filepath.Join(s.root, "b")
	basis.Targets[0].Parents[len(basis.Targets[0].Parents)-1].Inode++
	if got := s.Apply(ctx, request("a", Revision{}, "A")); got.Code != Applied {
		t.Fatal("caller mutation changed an admitted context proof")
	}
	if _, err := os.Stat(filepath.Join(s.root, "b")); !os.IsNotExist(err) {
		t.Fatal("caller changed admitted target")
	}
}
