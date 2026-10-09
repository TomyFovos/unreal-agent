//go:build linux || darwin

package mutation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestMutationClosedReasonsDoNotChangeCASOrPermissionCodes(t *testing.T) {
	s := service(t)
	for _, tc := range []struct {
		name    string
		request Request
		code    Code
		reason  Reason
	}{
		{"outside", request("../private-sensitive", Revision{}, "x"), Invalid, ReasonWorkspaceBoundary},
		{"parent-missing", request("absent-parent/a", Revision{}, "x"), Invalid, ReasonParentMissing},
		{"absent-with-digest", request("a", Revision{Digest: strings.Repeat("0", 64)}, "x"), Invalid, ReasonInvalidRevision},
		{"existing-missing-digest", request("a", Revision{Exists: true}, "x"), Invalid, ReasonInvalidRevision},
		{"stale", request("a", RevisionOf([]byte("does not exist")), "x"), Stale, ReasonRevisionConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s.Apply(t.Context(), tc.request)
			if r.Code != tc.code || r.Reason != tc.reason || r.Denial != nil {
				t.Fatal(r)
			}
			if _, err := os.Stat(filepath.Join(s.Root(), "a")); !os.IsNotExist(err) {
				t.Fatal("failed mutation wrote target", err)
			}
		})
	}
	if r := s.Apply(t.Context(), request("a", Revision{}, "created")); r.Code != Applied || r.Reason != "" {
		t.Fatal("valid absence did not create", r)
	}
	if err := os.Symlink(s.Root(), filepath.Join(s.Root(), "alias")); err != nil {
		t.Fatal(err)
	}
	if r := s.Apply(t.Context(), request("alias/b", Revision{}, "x")); r.Code != Invalid || r.Reason != ReasonUnsafeParent {
		t.Fatal("symlink boundary", r)
	}
	root := t.TempDir()
	private, err := New(Config{Root: root, StateDir: filepath.Join(root, "private-state"), Authorize: func(context.Context, string, bool) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := private.Path("private-state/receipt"); FailureReason(err) != ReasonInternalState || strings.Contains(err.Error(), "private-state") {
		t.Fatal("private state diagnostic", err)
	}
	s.authorize = func(context.Context, string, bool) error { return &permission.Error{Code: "denied"} }
	if r := s.Apply(t.Context(), request("denied", Revision{}, "x")); r.Code != Denied || r.Reason != ReasonPermissionDenied || r.Denial == nil {
		t.Fatal("denial changed into invalid", r)
	}
	if FailureReason(errors.New("raw private-sensitive /secret/path")) != ReasonTargetUnavailable || Reason("raw private-sensitive").Valid() {
		t.Fatal("raw error made public enum")
	}
}
