package permission

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	if err := c.SetMode(key, rev, mode); err != nil {
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
				select {
				case <-result:
					t.Fatal("detach answered pending request")
				default:
				}
				newKey, release := c.AcquireControl()
				defer release()
				if err := c.Decide(newKey, c.Snapshot().Revision, "op", false); err != nil {
					t.Fatal(err)
				}
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
	if fresh.Snapshot().Mode != ReadOnly || fresh.SetMode(key, rev, FullAccess) == nil {
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
	if c.SetMode(key, c.Snapshot().Revision, ReadOnly) == nil {
		t.Fatal("old attachment accepted")
	}
	encoded, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), key) || strings.Contains(string(encoded), newKey) {
		t.Fatal("private control leaked")
	}
	if !errors.Is(c.SetMode("", c.Snapshot().Revision, FullAccess), ErrControl) {
		t.Fatal("unauthenticated control")
	}
}
func TestHumanModesConfigAndUnrestrictedBaselineCannotBypassReadonly(t *testing.T) {
	for _, cfg := range []ModesConfig{{Enabled: true}} {
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

// A child's controller follows its parent's mode and takes its parent's
// answers, without a human lease (U17).
func TestChildControllerFollowsParentAndTakesAnswers(t *testing.T) {
	c, ctx, _ := humanController(t, Unrestricted(), time.Hour)
	if err := c.Follow(Ask); err != nil || c.Snapshot().Mode != Ask {
		t.Fatal("follow ask", err)
	}
	result := c.Request(ctx, Request{ID: "op", Tool: "Bash", Privileged: true})
	if len(c.Snapshot().Pending) != 1 {
		t.Fatal("request not pending")
	}
	if err := c.Answer("op", true); err != nil {
		t.Fatal(err)
	}
	if p := getPermit(t, result); p.Denial != nil || p.Decision.State != "allow_once" {
		t.Fatal("answer did not allow", p.Decision)
	}
	if c.Answer("op", true) == nil || c.Answer("missing", false) == nil {
		t.Fatal("answered a request that is not pending")
	}
	denied := c.Request(ctx, Request{ID: "op2", Tool: "Bash", Privileged: true})
	if err := c.Follow(FullAccess); err != nil {
		t.Fatal(err)
	}
	if p := getPermit(t, denied); p.Denial == nil || p.Decision.State != "revoked" {
		t.Fatal("a mode change must end a waiting request", p.Decision)
	}
	if p := getPermit(t, c.Request(ctx, Request{ID: "op3", Tool: "Bash", Privileged: true})); p.Denial != nil {
		t.Fatal("full access denied", p.Decision)
	}
}

// Orchestration holds the maximum in every mode and outlives mode changes.
func TestOrchestrationPermitHoldsMaximumAndSurvivesModeChanges(t *testing.T) {
	c, ctx, key := humanController(t, Unrestricted(), time.Hour)
	permit := getPermit(t, c.Request(ctx, Request{ID: "child", Tool: "SubagentStart", Orchestration: true}))
	if permit.Denial != nil {
		t.Fatal(permit.Decision)
	}
	defer permit.Release()
	if err := FromContext(permit.Context).CheckProcess(); err != nil {
		t.Fatal("orchestration must hold the maximum in Read Only", err)
	}
	humanMode(t, c, key, Ask)
	humanMode(t, c, key, FullAccess)
	if permit.Context.Err() != nil {
		t.Fatal("a mode change revoked orchestration")
	}
	ordinary := getPermit(t, c.Request(ctx, Request{ID: "read", Tool: "read"}))
	defer ordinary.Release()
	humanMode(t, c, key, ReadOnly)
	if ordinary.Context.Err() == nil {
		t.Fatal("ordinary Full Access work survived a mode change")
	}
}

// A read-only command runs in Read Only with the maximum's process
// permission, and a mode change does not revoke it.
func TestHumanReadOnlyCommandRunsInEveryMode(t *testing.T) {
	c, ctx, key := humanController(t, Unrestricted(), time.Second)
	for _, mode := range []Mode{ReadOnly, Ask, FullAccess} {
		if c.Snapshot().Mode != mode {
			humanMode(t, c, key, mode)
		}
		p := getPermit(t, c.Request(ctx, Request{ID: "git-" + string(mode), Tool: "Bash", ReadOnlyCommand: true, Scope: "$ git log"}))
		if p.Denial != nil || p.Context == nil {
			t.Fatalf("%s: %+v", mode, p)
		}
		if err := FromContext(p.Context).CheckProcess(); err != nil {
			t.Fatalf("%s: the command cannot start: %v", mode, err)
		}
		if mode == ReadOnly {
			humanMode(t, c, key, Ask)
			if p.Context.Err() != nil {
				t.Fatal("a mode change revoked a read-only command")
			}
			humanMode(t, c, key, ReadOnly)
		}
		p.Release()
	}
}

func TestHumanApprovalsHaveNoDefaultDeadlineOrPendingLimit(t *testing.T) {
	c, ctx, key := humanController(t, Unrestricted(), 0)
	humanMode(t, c, key, Ask)
	results := make([]<-chan Permit, 129)
	for i := range results {
		results[i] = c.Request(ctx, Request{ID: fmt.Sprint(i), Tool: "Bash", Scope: strings.Repeat("command", 100), Privileged: true})
		select {
		case <-results[i]:
			t.Fatal("request unexpectedly settled", i)
		default:
		}
	}
	snapshot := c.Snapshot()
	if len(snapshot.Pending) != 129 {
		t.Fatal("lost pending requests", len(snapshot.Pending))
	}
	for _, p := range snapshot.Pending {
		if !p.Deadline.IsZero() {
			t.Fatal("unexpected default deadline", p.Deadline)
		}
	}
	for i, result := range results {
		if err := c.Decide(key, c.Snapshot().Revision, fmt.Sprint(i), true); err != nil {
			t.Fatal(err)
		}
		permit := getPermit(t, result)
		if permit.Denial != nil {
			t.Fatal(permit.Decision)
		}
		permit.Release()
	}
}
