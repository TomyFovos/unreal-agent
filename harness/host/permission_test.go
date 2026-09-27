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
