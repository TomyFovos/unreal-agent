package permission

import (
	"context"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func humanController(t *testing.T, maximum *Policy, duration time.Duration) (*Controller, context.Context, string) {
	t.Helper()
	base := newPolicy(t, Config{Tools: []string{"read", "grep", "glob"}, ReadRoots: []string{t.TempDir()}})
	c := NewController(t.Context(), base, maximum, duration)
	t.Cleanup(c.Close)
	ctx := WithController(WithPolicy(t.Context(), maximum), c)
	key, release := c.AcquireControl()
	t.Cleanup(release)
	return c, ctx, key
}
func humanMode(t *testing.T, c *Controller, key string, mode Mode) {
	t.Helper()
	rev := c.Snapshot().Revision
	challenge := ""
	if mode == FullAccess {
		var err error
		challenge, err = c.PrepareFull(key, rev)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetMode(key, rev, mode, challenge); err != nil {
		t.Fatal(err)
	}
}
func getPermit(t *testing.T, result <-chan Permit) Permit {
	t.Helper()
	select {
	case p := <-result:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not settle")
		return Permit{}
	}
}
func TestHumanModesDefaultReadonlyAndMaximumDeny(t *testing.T) {
	c, ctx, _ := humanController(t, Unrestricted(), time.Second)
	if c.Snapshot().Mode != ReadOnly {
		t.Fatal("not read only")
	}
	for _, tool := range []string{"write", "edit", "Bash", "custom"} {
		p := getPermit(t, c.Request(ctx, Request{ID: tool, Tool: tool, Privileged: true}))
		if p.Denial == nil || p.Context != nil || p.Decision.State != "read_only" {
			t.Fatal(p)
		}
	}
	assertDenied(t, FromContext(ctx).CheckProcess(), Denied)
	assertDenied(t, FromContext(ctx).CheckPath(filepath.Join(t.TempDir(), "no"), true), Denied)
	upper := newPolicy(t, Config{Tools: []string{"read"}})
	d, bound, key := humanController(t, upper, time.Second)
	humanMode(t, d, key, FullAccess)
	p := getPermit(t, d.Request(bound, Request{ID: "denied", Tool: "Bash", Privileged: true}))
	if p.Denial == nil {
		t.Fatal("upper deny overridden")
	}
}
func TestHumanAllowOnceDenyTimeoutCancellationAndDetach(t *testing.T) {
	for _, which := range []string{"allow", "deny", "timeout", "cancel", "detach"} {
		t.Run(which, func(t *testing.T) {
			c, ctx, key := humanController(t, Unrestricted(), 50*time.Millisecond)
			humanMode(t, c, key, Ask)
			child, cancel := context.WithCancel(ctx)
			defer cancel()
			result := c.Request(child, Request{ID: "op", Tool: "Bash", Privileged: true, Scope: "ambient"})
			before := c.Snapshot()
			if len(before.Pending) != 1 {
				t.Fatal("no pending approval")
			}
			switch which {
			case "allow", "deny":
				if err := c.Decide(key, before.Revision, "op", which == "allow"); err != nil {
					t.Fatal(err)
				}
				if c.Decide(key, before.Revision, "op", true) == nil {
					t.Fatal("replayed approval accepted")
				}
			case "cancel":
				cancel()
			case "detach":
				_, release := c.AcquireControl()
				release()
			}
			p := getPermit(t, result)
			defer p.Release()
			if which == "allow" {
				if p.Denial != nil || p.Decision.State != "allow_once" || FromContext(p.Context).CheckProcess() != nil {
					t.Fatal(p)
				}
				assertDenied(t, FromContext(ctx).CheckProcess(), Denied)
				if len(c.Snapshot().Pending) != 0 {
					t.Fatal("old request retained")
				}
			} else if p.Denial == nil || p.Context != nil {
				t.Fatal("missing closed denial", p)
			}
		})
	}
}
func TestHumanFullConfirmationReplayRevocationAndFreshLifetime(t *testing.T) {
	c, ctx, key := humanController(t, Unrestricted(), time.Second)
	rev := c.Snapshot().Revision
	for _, proof := range []string{"", "true", "config approval", "LLM approval"} {
		if c.SetMode(key, rev, FullAccess, proof) == nil {
			t.Fatal("forged confirmation")
		}
	}
	humanMode(t, c, key, FullAccess)
	result := getPermit(t, c.Request(ctx, Request{ID: "process", Tool: "Bash", Privileged: true}))
	defer result.Release()
	if result.Denial != nil || FromContext(result.Context).CheckProcess() != nil {
		t.Fatal("full access denied")
	}
	humanMode(t, c, key, ReadOnly)
	if result.Context.Err() == nil {
		t.Fatal("revocation did not cancel running operation")
	}
	assertDenied(t, FromContext(result.Context).CheckProcess(), Denied)
	fresh, _, _ := humanController(t, Unrestricted(), time.Second)
	if fresh.Snapshot().Mode != ReadOnly || fresh.SetMode(key, rev, FullAccess, "old") == nil {
		t.Fatal("restored authority")
	}
}
func TestHumanLeaseIsolationParentDenyAndNoSecretProjection(t *testing.T) {
	c, ctx, key := humanController(t, Unrestricted(), time.Second)
	humanMode(t, c, key, Ask)
	result := c.Request(WithPolicy(ctx, DenyAll()), Request{ID: "restricted", Tool: "Bash", Privileged: true})
	rev := c.Snapshot().Revision
	if c.Decide("other-session", rev, "restricted", true) == nil {
		t.Fatal("cross Session control")
	}
	if c.Decide(key, rev, "restricted", true) != nil {
		t.Fatal("valid control denied")
	}
	p := getPermit(t, result)
	defer p.Release()
	assertDenied(t, FromContext(p.Context).CheckProcess(), Denied)
	newKey, release := c.AcquireControl()
	defer release()
	if c.SetMode(key, c.Snapshot().Revision, ReadOnly, "") == nil {
		t.Fatal("old attachment accepted")
	}
	encoded, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), key) || strings.Contains(string(encoded), newKey) {
		t.Fatal("private control leaked")
	}
	if !errors.Is(c.SetMode("", c.Snapshot().Revision, FullAccess, ""), ErrControl) {
		t.Fatal("unauthenticated control")
	}
}
func TestHumanModesConfigAndUnrestrictedBaselineCannotBypassReadonly(t *testing.T) {
	for _, cfg := range []ModesConfig{{Enabled: true}, {ApprovalTimeoutMillis: -1}, {ApprovalTimeoutMillis: 3600001}} {
		if cfg.Validate() == nil {
			t.Fatal("bad config accepted")
		}
	}
	ctx := t.Context()
	c := NewController(ctx, Unrestricted(), Unrestricted(), time.Second)
	defer c.Close()
	ctx = WithController(ctx, c)
	assertDenied(t, FromContext(ctx).CheckProcess(), Denied)
	assertDenied(t, FromContext(ctx).CheckPath(filepath.Join(t.TempDir(), "x"), true), Denied)
}
