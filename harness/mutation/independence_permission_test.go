//go:build linux || darwin

package mutation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// This records the Permission boundary itself: a denied final mutation result
// cannot undo an earlier unauthorized read/hash.
func TestIndependenceDeniedAuthorizationHasNoBodyIO(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "a"), "synthetic uncertain")
	independenceWrite(t, filepath.Join(s.root, "b"), "synthetic before")
	var authorizations, reads, hashes int
	s.authorize = func(context.Context, string, bool) error {
		authorizations++
		return errors.New("synthetic denial")
	}
	s.observeIO = func(_ string, phase string) {
		switch phase {
		case "read":
			reads++
		case "hash":
			hashes++
		}
	}
	_, err := s.ProveIndependent(t.Context(), request("b", RevisionOf([]byte("synthetic before")), "synthetic after"), []string{"a"})
	if err == nil || authorizations == 0 || reads != 0 || hashes != 0 {
		t.Fatalf("permission boundary: err=%v authorizations=%d body_reads=%d body_hashes=%d", err, authorizations, reads, hashes)
	}
}

func TestIndependenceEachCurrentAuthorityPrecedesBodyIO(t *testing.T) {
	for _, denied := range []string{"a_metadata", "b_mutation"} {
		t.Run(denied, func(t *testing.T) {
			s := service(t)
			a, b := filepath.Join(s.root, "a"), filepath.Join(s.root, "b")
			independenceWrite(t, a, "uncertain")
			independenceWrite(t, b, "before")
			var deniedCalls, reads, hashes int
			s.authorize = func(_ context.Context, path string, write bool) error {
				if denied == "a_metadata" && path == a && !write || denied == "b_mutation" && path == b && write {
					deniedCalls++
					return errors.New("synthetic current authority denied")
				}
				return nil
			}
			s.observeIO = func(_ string, phase string) {
				if phase == "read" {
					reads++
				}
				if phase == "hash" {
					hashes++
				}
			}
			r := request("b", RevisionOf([]byte("before")), "after")
			if _, err := s.ProveIndependent(t.Context(), r, []string{"a"}); err == nil || deniedCalls == 0 || reads != 0 || hashes != 0 {
				t.Fatalf("proof observed target bodies before current %s authority: err=%v denied_calls=%d reads=%d hashes=%d", denied, err, deniedCalls, reads, hashes)
			}
			deniedCalls = 0
			got := s.Apply(WithIndependenceRequirement(t.Context(), []string{"a"}), r)
			if got.Code == Applied || deniedCalls == 0 || reads != 0 || hashes != 0 {
				t.Fatalf("mutation observed target bodies before current %s authority: code=%s denied_calls=%d reads=%d hashes=%d", denied, got.Code, deniedCalls, reads, hashes)
			}
			independenceContents(t, b, "before")
		})
	}
}

func TestIndependencePendingAndRevokedPermissionHasNoBodyIO(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "a"), "uncertain")
	independenceWrite(t, filepath.Join(s.root, "b"), "before")
	policy, err := permission.New(permission.Config{Tools: []string{"write", "edit"}, ReadRoots: []string{s.root}, WriteRoots: []string{s.root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policy.Close() })
	controller := permission.NewController(t.Context(), policy, policy, time.Minute)
	t.Cleanup(controller.Close)
	ctx := permission.WithController(t.Context(), controller)
	key, release := controller.AcquireControl()
	t.Cleanup(release)
	if err := controller.SetMode(key, controller.Snapshot().Revision, permission.Ask, ""); err != nil {
		t.Fatal(err)
	}
	pending := controller.Request(ctx, permission.Request{ID: "synthetic-write", Tool: "write", Privileged: true})
	if len(controller.Snapshot().Pending) != 1 {
		t.Fatal("fixture has no pending Ask request")
	}
	var authorizations, reads, hashes int
	s.authorize = func(ctx context.Context, path string, write bool) error {
		authorizations++
		return permission.FromContext(ctx).CheckPath(path, write)
	}
	s.observeIO = func(_ string, phase string) {
		if phase == "read" {
			reads++
		}
		if phase == "hash" {
			hashes++
		}
	}
	r := request("b", RevisionOf([]byte("before")), "after")
	if _, err := s.ProveIndependent(ctx, r, []string{"a"}); err == nil || authorizations == 0 || reads != 0 || hashes != 0 {
		t.Fatalf("pending Ask accessed body: err=%v authorizations=%d reads=%d hashes=%d", err, authorizations, reads, hashes)
	}
	if err := controller.Decide(key, controller.Snapshot().Revision, "synthetic-write", true); err != nil {
		t.Fatal(err)
	}
	permit := <-pending
	t.Cleanup(permit.Release)
	if permit.Denial != nil || permit.Context == nil {
		t.Fatal("approved fixture has no operation lease")
	}
	if err := controller.SetMode(key, controller.Snapshot().Revision, permission.ReadOnly, ""); err != nil {
		t.Fatal(err)
	}
	if permit.Context.Err() == nil {
		t.Fatal("revocation did not invalidate the old lease")
	}
	reads, hashes = 0, 0
	if _, err := s.ProveIndependent(permit.Context, r, []string{"a"}); err == nil || reads != 0 || hashes != 0 {
		t.Fatalf("revoked lease accessed body: err=%v reads=%d hashes=%d", err, reads, hashes)
	}
	if _, err := s.SnapshotForMutation(permit.Context, "b"); err == nil || reads != 0 || hashes != 0 {
		t.Fatalf("revoked mutation snapshot accessed body: err=%v reads=%d hashes=%d", err, reads, hashes)
	}
}

func TestIndependentWriteAndEditUseWriteGrantAndNeverReadBlockedBody(t *testing.T) {
	s := service(t)
	a, b := filepath.Join(s.root, "same/a"), filepath.Join(s.root, "same/b")
	independenceWrite(t, a, "uncertain")
	blocked, err := s.CaptureFootprint(t.Context(), []string{"same/a"})
	if err != nil {
		t.Fatal(err)
	}
	var aReads, aHashes, bReadGrants, aMetadataGrants, bWriteGrants int
	s.authorize = func(_ context.Context, path string, write bool) error {
		switch {
		case path == a && !write:
			aMetadataGrants++
			return nil
		case path == b && write:
			bWriteGrants++
			return nil
		case path == b && !write:
			bReadGrants++
		}
		return errors.New("synthetic scope denial")
	}
	s.observeIO = func(path, phase string) {
		if path == a && phase == "read" {
			aReads++
		}
		if path == a && phase == "hash" {
			aHashes++
		}
	}
	ctx := WithBlockedFootprints(WithIndependenceRequirement(t.Context(), []string{"same/a"}), []Footprint{blocked})
	write := s.Execute(ctx, "synthetic-write-only-b", request("same/b", Revision{}, "before"))
	if write.Code != Applied || write.Targets[0].Revision != RevisionOf([]byte("before")) {
		t.Fatal("independent Write did not complete", write)
	}
	edit := s.ExecutePrepared(ctx, "synthetic-edit-only-b", []byte("synthetic-edit-plan"), func(ctx context.Context) (Request, error) {
		snapshot, err := s.SnapshotForMutation(ctx, "same/b")
		if err != nil || string(snapshot.Data) != "before" {
			return Request{}, errors.New("synthetic edit preparation failed")
		}
		return request("same/b", snapshot.Revision, "after"), nil
	})
	if edit.Code != Applied || edit.Targets[0].Revision != RevisionOf([]byte("after")) {
		t.Fatal("independent Edit did not complete", edit)
	}
	if aReads != 0 || aHashes != 0 || bReadGrants != 0 || aMetadataGrants == 0 || bWriteGrants == 0 {
		t.Fatalf("wrong authority/I/O: a_reads=%d a_hashes=%d b_read_grants=%d a_metadata_grants=%d b_write_grants=%d", aReads, aHashes, bReadGrants, aMetadataGrants, bWriteGrants)
	}
	independenceContents(t, b, "after")
	independenceContents(t, a, "uncertain")
	for _, id := range []string{"synthetic-write-only-b", "synthetic-edit-only-b"} {
		if receipt, exists, err := s.Recover(t.Context(), id); err != nil || !exists || receipt.Code != Applied {
			t.Fatal("independent mutation has no durable applied receipt", id)
		}
	}
}

func TestMutationSnapshotAndFootprintNeedOnlyCurrentWriteAuthority(t *testing.T) {
	s := service(t)
	independenceWrite(t, filepath.Join(s.root, "b"), "before")
	policy, err := permission.New(permission.Config{WriteRoots: []string{s.root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policy.Close() })
	ctx := permission.WithPolicy(t.Context(), policy)
	s.authorize = func(ctx context.Context, path string, write bool) error {
		return permission.FromContext(ctx).CheckPath(path, write)
	}
	if _, err := s.Snapshot(ctx, "b"); permission.Failure(err) == nil {
		t.Fatal("ordinary Read unexpectedly inherited the write grant")
	}
	if snapshot, err := s.SnapshotForMutation(ctx, "b"); err != nil || snapshot.Revision != RevisionOf([]byte("before")) {
		t.Fatal("mutation revision requested an extra Read grant", err)
	}
	footprint, err := s.CaptureFootprint(ctx, []string{"b"})
	if err != nil || s.ValidateExecutionFootprint(ctx, footprint) != nil {
		t.Fatal("execution metadata requested an extra Read grant", err)
	}
	if err := s.ValidateFootprint(ctx, footprint); err == nil || permission.Failure(err) != nil {
		t.Fatal("historical metadata reused the old write grant")
	}
	denied := permission.WithPolicy(t.Context(), permission.DenyAll())
	if _, err := s.CaptureFootprint(denied, []string{"b"}); permission.Failure(err) == nil {
		t.Fatal("ordinary execution capture lost its typed permission denial", err)
	}
	if err := s.ValidateExecutionFootprint(denied, footprint); permission.Failure(err) == nil {
		t.Fatal("ordinary execution validation lost its typed permission denial", err)
	}
}

func TestFootprintDenialHasNoTargetMetadataOrBodyIO(t *testing.T) {
	for _, action := range []string{"capture", "historical_validate", "execution_validate"} {
		t.Run(action, func(t *testing.T) {
			s := service(t)
			independenceWrite(t, filepath.Join(s.root, "a"), "synthetic")
			footprint, err := s.CaptureFootprint(t.Context(), []string{"a"})
			if err != nil {
				t.Fatal(err)
			}
			var authorizations, observations int
			s.authorize = func(context.Context, string, bool) error { authorizations++; return errors.New("synthetic denial") }
			s.observeIO = func(string, string) { observations++ }
			switch action {
			case "capture":
				_, err = s.CaptureFootprint(t.Context(), []string{"a"})
			case "historical_validate":
				err = s.ValidateFootprint(t.Context(), footprint)
			case "execution_validate":
				err = s.ValidateExecutionFootprint(t.Context(), footprint)
			}
			if err == nil || authorizations == 0 || observations != 0 {
				t.Fatalf("metadata bypass: err=%v authorizations=%d observations=%d", err, authorizations, observations)
			}
		})
	}
}

func TestFootprintUsesPolicyApprovedParentDescriptor(t *testing.T) {
	for _, action := range []string{"capture", "historical_validate", "execution_validate"} {
		t.Run(action, func(t *testing.T) {
			s := service(t)
			independenceWrite(t, filepath.Join(s.root, "a"), "original")
			footprint, err := s.CaptureFootprint(t.Context(), []string{"a"})
			if err != nil {
				t.Fatal(err)
			}
			policy, err := permission.New(permission.Config{ReadRoots: []string{s.root}, WriteRoots: []string{s.root}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { policy.Close() })
			ctx := permission.WithPolicy(t.Context(), policy)
			var observations, authorizations int
			s.observeIO = func(string, string) { observations++ }
			s.authorize = func(ctx context.Context, path string, write bool) error {
				authorizations++
				if err := permission.FromContext(ctx).CheckPath(path, write); err != nil {
					return err
				}
				if err := os.Rename(s.root, s.root+"-old"); err != nil {
					return err
				}
				independenceWrite(t, filepath.Join(s.root, "a"), "decoy")
				return nil
			}
			switch action {
			case "capture":
				_, err = s.CaptureFootprint(ctx, []string{"a"})
			case "historical_validate":
				err = s.ValidateFootprint(ctx, footprint)
			case "execution_validate":
				err = s.ValidateExecutionFootprint(ctx, footprint)
			}
			if err == nil || authorizations == 0 || observations != 0 {
				t.Fatalf("replacement bypassed approved descriptor: err=%v authorizations=%d target_observations=%d", err, authorizations, observations)
			}
			independenceContents(t, filepath.Join(s.root, "a"), "decoy")
			independenceContents(t, filepath.Join(s.root+"-old", "a"), "original")
		})
	}
}

func TestIndependenceOversizedBlockedBodyNeedsNoRead(t *testing.T) {
	s := service(t)
	path := filepath.Join(s.root, "a")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxFileBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	var aBodyIO int
	s.observeIO = func(observed, phase string) {
		if observed == path && (phase == "read" || phase == "hash") {
			aBodyIO++
		}
	}
	if result := s.Apply(WithIndependenceRequirement(t.Context(), []string{"a"}), request("b", Revision{}, "independent")); result.Code != Applied || aBodyIO != 0 {
		t.Fatal("finite metadata separation unnecessarily read A", result, aBodyIO)
	}
}

func TestIndependenceRevisionObservationParticipatesInMutationLocks(t *testing.T) {
	for _, action := range []string{"prove", "revalidate"} {
		t.Run(action, func(t *testing.T) {
			s := service(t)
			independenceWrite(t, filepath.Join(s.root, "a"), "uncertain")
			independenceWrite(t, filepath.Join(s.root, "b"), "before")
			r := request("b", RevisionOf([]byte("before")), "after")
			fence, err := s.ProveIndependent(t.Context(), r, []string{"a"})
			if err != nil {
				t.Fatal(err)
			}
			lock, err := s.lock(t.Context(), filepath.Join(s.root, "b"))
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			var bodyIO int
			s.observeIO = func(_ string, phase string) {
				if phase == "read" || phase == "hash" {
					bodyIO++
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			if action == "prove" {
				_, err = s.ProveIndependent(ctx, r, []string{"a"})
			} else {
				err = fence.Revalidate(ctx)
			}
			if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || bodyIO != 0 {
				t.Fatalf("%s bypassed existing target lock: err=%v context=%v body_io=%d", action, err, ctx.Err(), bodyIO)
			}
		})
	}
}
