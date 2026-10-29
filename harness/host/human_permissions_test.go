package host

import (
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func modeHost(t *testing.T, directory string) *Host {
	t.Helper()
	h, err := New(t.Context(), Config{Directory: directory, Build: factory(modelFunc(echo)), PermissionMaximum: permission.Unrestricted(), ApprovalTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func controlMode(t *testing.T, c *permission.Controller, key string, mode permission.Mode) {
	t.Helper()
	rev := c.Snapshot().Revision
	if err := c.SetMode(key, rev, mode); err != nil {
		t.Fatal(err)
	}
}
func waitOperation(t *testing.T, s *Session, id operation.ID, predicate func(operation.Operation) bool) operation.Operation {
	t.Helper()
	ctx := timeout(t)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		v, err := s.Inspect(0, 4096)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range v.Operations {
			if op.ID == id && predicate(op) {
				return op
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("operation checkpoint not observed")
		case <-tick.C:
		}
	}
}
func humanShell(t *testing.T, root string) operation.Spec {
	t.Helper()
	s, err := operation.NewShellSpec(operation.ShellInput{Shell: "/bin/sh", Directory: root, Command: "printf one >> marker"}, t.TempDir(), 256)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The selected mode survives detach and a Host restart (U20); approvals and
// the old generation's control do not.
func TestHumanHostFullAccessDetachAttachAndRestartKeepsTheMode(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	h := modeHost(t, dir)
	s, err := h.Create(t.Context(), Options{ID: "human", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.PermissionControl(s.Generation)
	if err != nil {
		t.Fatal(err)
	}
	key, detach := c.AcquireControl()
	controlMode(t, c, key, permission.FullAccess)
	detach()
	attached, err := h.Attach(s.ID)
	if err != nil || attached != s || c.Snapshot().Mode != permission.FullAccess {
		t.Fatal("detach lost Session grant")
	}
	spec := humanShell(t, root)
	if _, err = s.SubmitOperation(timeout(t), s.Generation, "work", "work", "Bash", spec); err != nil {
		t.Fatal(err)
	}
	result := waitOperation(t, s, "work", func(o operation.Operation) bool { return o.Status == operation.StatusCompleted })
	if result.Permission == nil || result.Permission.State != "full_access" {
		t.Fatal("grant not canonical", result)
	}
	before, _ := s.Inspect(0, 4096)
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	stopped, _ := s.Inspect(0, 4096)
	if !stopped.Permissions.Stopped || stopped.Permissions.Mode != permission.ReadOnly || stopped.Permissions.SelectedMode != permission.FullAccess {
		t.Fatal("stopped Host reports effective historical grant")
	}
	next := modeHost(t, dir)
	r, err := next.Resume(t.Context(), Options{ID: s.ID, Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	view, _ := r.Inspect(0, 4096)
	if !view.Permissions.Enabled || view.Permissions.Stopped || view.Permissions.Mode != permission.FullAccess || len(view.History.Items) < len(before.History.Items) {
		t.Fatal("restart lost the selected mode or history")
	}
	if _, err = r.PermissionControl(s.Generation); err == nil {
		t.Fatal("old generation accepted")
	}
	if _, err = r.SubmitOperation(timeout(t), r.Generation, "new", "new", "Bash", spec); err != nil {
		t.Fatal(err)
	}
	waitOperation(t, r, "new", func(o operation.Operation) bool { return o.Status == operation.StatusCompleted })
	content, err := os.ReadFile(filepath.Join(root, "marker"))
	if err != nil || string(content) != "oneone" {
		t.Fatal("old work repeated or new work missing", err, string(content))
	}
}
func TestHumanHostPendingApprovalRecoveryDoesNotExecuteOrDuplicate(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	h := modeHost(t, dir)
	s, err := h.Create(t.Context(), Options{ID: "pending", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.PermissionControl(s.Generation)
	key, _ := c.AcquireControl()
	controlMode(t, c, key, permission.Ask)
	spec := humanShell(t, root)
	first, err := s.SubmitOperation(timeout(t), s.Generation, "one-input", "one-op", "Bash", spec)
	if err != nil {
		t.Fatal(err)
	}
	waitOperation(t, s, "one-op", func(o operation.Operation) bool { return o.Permission != nil && o.Permission.State == "pending" })
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	next := modeHost(t, dir)
	r, err := next.Resume(t.Context(), Options{ID: s.ID, Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	result := waitOperation(t, r, "one-op", func(o operation.Operation) bool {
		return o.Status == operation.StatusCanceled || o.Status == operation.StatusFailed
	})
	if result.Status != operation.StatusCanceled || result.Permission == nil || result.Permission.State != "pending" {
		t.Fatal("resume reused approval", result)
	}
	replayed, err := r.SubmitOperation(timeout(t), r.Generation, "one-input", "one-op", "Bash", spec)
	if err != nil || replayed != first {
		t.Fatal("input replay duplicated receipt", err)
	}
	view, _ := r.Inspect(0, 4096)
	inputs := 0
	for _, item := range view.History.Items {
		if item.Kind == sessionstore.ItemInput {
			if item.Data.(inbox.Input).ID == "one-input" {
				inputs++
			}
		}
	}
	if inputs != 1 || len(view.Operations) != 1 {
		t.Fatal("canonical duplication", inputs, len(view.Operations))
	}
	if _, err = os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("pending work executed")
	}
}
func TestHumanHostUpperParentDenySurvivesFullApproval(t *testing.T) {
	h, err := New(permission.WithPolicy(t.Context(), permission.DenyAll()), Config{Directory: t.TempDir(), Build: factory(modelFunc(echo)), PermissionMaximum: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), Options{ID: "denied", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.PermissionControl(s.Generation)
	key, release := c.AcquireControl()
	defer release()
	controlMode(t, c, key, permission.FullAccess)
	root := t.TempDir()
	if _, err = s.SubmitOperation(timeout(t), s.Generation, "denied", "denied", "Bash", humanShell(t, root)); err != nil {
		t.Fatal(err)
	}
	result := waitOperation(t, s, "denied", func(o operation.Operation) bool { return o.Status == operation.StatusFailed })
	if result.Denial == nil {
		t.Fatal("human grant overrode parent deny")
	}
	if _, err = os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatal("denied side effect")
	}
}

// The workspace's last selected mode survives a Host restart, an update and
// new Sessions (U20).
func TestSelectedModeSurvivesHostRestart(t *testing.T) {
	directory := t.TempDir()
	h := modeHost(t, directory)
	s, err := h.Create(t.Context(), Options{ID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.PermissionControl(s.Generation)
	if err != nil {
		t.Fatal(err)
	}
	key, release := c.AcquireControl()
	controlMode(t, c, key, permission.FullAccess)
	release()
	stop(t, s)
	h.Close()
	h = modeHost(t, directory)
	for _, id := range []string{"first", "second"} {
		var s *Session
		if id == "first" {
			s, err = h.Resume(t.Context(), Options{ID: session.ID(id)})
		} else {
			s, err = h.Create(t.Context(), Options{ID: session.ID(id)})
		}
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.PermissionControl(s.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Snapshot().Mode; got != permission.FullAccess {
			t.Fatalf("%s started in %s after a restart", id, got)
		}
		stop(t, s)
	}
}
