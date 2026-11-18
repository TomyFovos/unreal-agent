package host

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"testing"
)

func TestHostPolicyExplicitDefaultAndParentIntersection(t *testing.T) {
	cases := []struct {
		name             string
		parent, selected *permission.Policy
		allowed          bool
	}{
		{"default-denied", nil, nil, false},
		{"explicit-unrestricted", nil, permission.Unrestricted(), true},
		{"parent-cannot-widen", permission.DenyAll(), permission.Unrestricted(), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.parent != nil {
				ctx = permission.WithPolicy(ctx, tt.parent)
			}
			observed := false
			h, err := New(ctx, Config{Directory: t.TempDir(), Build: func(ctx context.Context, id session.ID) (Runtime, error) {
				observed = permission.FromContext(ctx).CheckProcess() == nil
				return factory(modelFunc(echo))(ctx, id)
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			s, err := h.Create(t.Context(), Options{ID: "policy", Policy: tt.selected})
			if err != nil {
				t.Fatal(err)
			}
			if observed != tt.allowed {
				t.Fatalf("process allowed=%v want=%v", observed, tt.allowed)
			}
			stop(t, s)
		})
	}
}

func TestResumeReevaluatesHostPolicy(t *testing.T) {
	var permissions []bool
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: func(ctx context.Context, id session.ID) (Runtime, error) {
		permissions = append(permissions, permission.FromContext(ctx).CheckProcess() == nil)
		return factory(modelFunc(echo))(ctx, id)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), Options{ID: "narrow", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	stop(t, s)
	s, err = h.Resume(t.Context(), Options{ID: "narrow", Policy: permission.DenyAll()})
	if err != nil {
		t.Fatal(err)
	}
	stop(t, s)
	if len(permissions) != 2 || !permissions[0] || permissions[1] {
		t.Fatal("resume retained stale privileges", permissions)
	}
}

func TestCapabilityPreflightFromOwnerContextPreservesRestrictions(t *testing.T) {
	maximum := permission.Unrestricted()
	controller := permission.NewController(t.Context(), permission.DenyAll(), maximum, 0)
	defer controller.Close()
	ctx := permission.WithController(permission.WithPolicy(t.Context(), maximum), controller)
	s := &Session{ctx: ctx}
	if permission.FromContext(ctx).CheckProcess() == nil {
		t.Fatal("execution context unexpectedly allows process creation")
	}
	if err := permission.FromContext(s.WithCapabilityPolicy(ctx)).CheckProcess(); err != nil {
		t.Fatal("owner's current mode narrowed capability preflight", err)
	}
	if permission.FromContext(ctx).CheckProcess() == nil {
		t.Fatal("preflight granted execution permission")
	}
	narrow := permission.WithPolicy(ctx, permission.DenyAll())
	if permission.FromContext(s.WithCapabilityPolicy(narrow)).CheckProcess() == nil {
		t.Fatal("capability preflight widened an immutable caller restriction")
	}
}
