//go:build linux || darwin

package mutation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotParticipatesInMutationLock(t *testing.T) {
	s := service(t)
	if r := s.Apply(t.Context(), request("a", Revision{}, "old")); r.Code != Applied {
		t.Fatal(r)
	}
	lock, err := s.lock(t.Context(), filepath.Join(s.Root(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err = s.Snapshot(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("snapshot bypassed mutation lock: %v", err)
	}
}
