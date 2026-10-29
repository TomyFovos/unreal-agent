package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestStoppedPermissionDisplayCannotGrant(t *testing.T) {
	s := Snapshot{Permissions: permission.ModeSnapshot{Enabled: true, Mode: permission.ReadOnly, SelectedMode: permission.FullAccess, Stopped: true}}
	if p := permissionPicker(s); len(p.Options) != 0 || !strings.Contains(p.Title, "停止中") {
		t.Fatal("stopped picker permits privilege selection")
	}
	if !strings.Contains(permissionStatus(s), "Session stopped") || permissionBadge(s) == "FULL " {
		t.Fatal("stopped status claims active Full Access")
	}
}

func humanUI(t *testing.T) (*permission.Controller, chan Key, *screenObserver, *fakeClient, <-chan error) {
	t.Helper()
	c := permission.NewController(t.Context(), permission.Unrestricted(), permission.Unrestricted(), time.Second)
	t.Cleanup(c.Close)
	f, keys, screen := newFake(), make(chan Key, 32), &screenObserver{}
	theme := Theme{Plain: true}
	done := make(chan error, 1)
	watch := func(ctx context.Context, generation string) (permission.ControlConnection, error) {
		key, release := c.AcquireControl()
		updates := make(chan permission.ModeSnapshot, 1)
		go func() {
			defer close(updates)
			defer release()
			for {
				s, changed := c.Watch()
				select {
				case updates <- s:
				case <-ctx.Done():
					return
				}
				select {
				case <-changed:
				case <-ctx.Done():
					return
				}
			}
		}()
		return permission.ControlConnection{Initial: c.Snapshot(), Updates: updates, Cancel: release,
			Request: func(ctx context.Context, r permission.ControlRequest) (permission.ControlReply, error) {
				var out permission.ControlReply
				var err error
				switch r.Action {
				case "set_mode":
					err = c.SetMode(key, r.Expected, r.Mode)
				case "decide":
					err = c.Decide(key, r.Expected, r.Operation, r.Allow)
				}
				out.Snapshot = c.Snapshot()
				return out, err
			}}, nil
	}
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: screen, Theme: &theme, Size: func() (int, int) { return 80, 24 }, WatchPermissions: watch})
	}()
	waitFor(t, func() bool { return strings.Contains(screen.text(), "[Read Only]") })
	return c, keys, screen, f, done
}

func stopHumanUI(t *testing.T, keys chan Key, done <-chan error) {
	t.Helper()
	keys <- Key{Name: "detach"}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("detach hung")
	}
}

func TestHumanTUIFullSelectionAppliesImmediatelyAndCanRevoke(t *testing.T) {
	c, keys, screen, f, done := humanUI(t)
	defer stopHumanUI(t, keys, done)
	keys <- Key{Text: "/permissions"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(screen.text(), "権限を選んでください") })
	keys <- Key{Name: "down"}
	keys <- Key{Name: "down"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return c.Snapshot().Mode == permission.FullAccess && strings.Contains(screen.text(), "[Full Access]")
	})
	keys <- Key{Text: "/permissions"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(screen.text(), "権限を選んでください") })
	keys <- Key{Name: "up"}
	keys <- Key{Name: "up"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return c.Snapshot().Mode == permission.ReadOnly })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("permissions control became chat")
	}
}

func TestHumanTUIOnceDenyAndDetachNeverApprovePending(t *testing.T) {
	for _, action := range []string{"allow", "deny", "detach"} {
		t.Run(action, func(t *testing.T) {
			c, keys, screen, _, done := humanUI(t)
			keys <- Key{Text: "/permissions"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(screen.text(), "権限を選んでください") })
			keys <- Key{Name: "down"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool {
				return c.Snapshot().Mode == permission.Ask && strings.Contains(screen.text(), "[Ask]")
			})
			operationCtx, cancelOperation := context.WithCancel(t.Context())
			defer cancelOperation()
			ctx := permission.WithController(permission.WithPolicy(operationCtx, permission.Unrestricted()), c)
			result := c.Request(ctx, permission.Request{ID: "pending-op", Tool: "Bash", Privileged: true, Scope: "AMBIENT filesystem, network, environment, credentials and shared mounts; no OS sandbox"})
			waitFor(t, func() bool {
				return strings.Contains(screen.text(), "Allow once") && strings.Contains(screen.text(), "Allow Bash?") && !strings.Contains(screen.text(), "pending-op") && strings.Contains(screen.text(), "AMBIENT")
			})
			if action == "detach" {
				stopHumanUI(t, keys, done)
				select {
				case <-result:
					t.Fatal("detach answered pending operation")
				default:
				}
				cancelOperation()
			} else {
				// Allow once is selected first; Deny is one step down.
				if action == "deny" {
					keys <- Key{Name: "down"}
				}
				keys <- Key{Name: "enter"}
			}
			select {
			case grant := <-result:
				if (grant.Denial == nil) != (action == "allow") {
					t.Fatal("unexpected permission outcome", grant.Decision)
				}
				if grant.Release != nil {
					grant.Release()
				}
			case <-time.After(2 * time.Second):
				t.Fatal("permission decision did not finish")
			}
			if action != "detach" {
				stopHumanUI(t, keys, done)
			}
		})
	}
}

func TestHumanTUIPermissionProjectionAndSmallDisplay(t *testing.T) {
	s := idleSnapshot()
	s.Permissions = permission.ModeSnapshot{Enabled: true, Mode: permission.ReadOnly, Revision: 5}
	for _, size := range [][2]int{{80, 24}, {16, 8}, {160, 40}} {
		u := plainUI()
		u.Picker = operationPermissionConfirmation(s, permission.Pending{ID: "op", Tool: "Bash", Scope: "echo test"})
		if size[0] == 160 {
			u.View = ViewSplit
		}
		frame := RenderFrame(s, u, size[0], size[1])
		bounds(t, frame, size[0], size[1])
		if size[0] == 16 && !strings.Contains(frameText(frame), "Allow once") {
			t.Fatal("small terminal hid approval options")
		}
		if strings.Contains(frameText(frame), "private-control-challenge") {
			t.Fatal("private control key displayed")
		}
	}
	m := NewModel("s")
	m.state.Generation = "current"
	m.state.Permissions = s.Permissions
	m.setPermissions("old", permission.ModeSnapshot{Enabled: true, Mode: permission.FullAccess, Revision: 8})
	m.setPermissions("current", permission.ModeSnapshot{Enabled: true, Mode: permission.FullAccess, Revision: 4})
	if m.Snapshot().Permissions.Mode != permission.ReadOnly {
		t.Fatal("stale projection changed current mode")
	}
	confirm := operationPermissionConfirmation(s, permission.Pending{ID: "op", Tool: "write", Scope: "Claude Code Write: /w/a.txt"})
	if confirm.Title != "Allow Write?" || confirm.Instructions[0] != "Claude Code Write: /w/a.txt" {
		t.Fatalf("confirmation does not lead with the action: %q %q", confirm.Title, confirm.Instructions)
	}
	if !permissionControlAction(confirm).Allow {
		t.Fatal("confirmation must start on Allow once")
	}
	confirm.move(1)
	if permissionControlAction(confirm).Allow || permissionGrants(confirm) {
		t.Fatal("Deny must deny")
	}

}
