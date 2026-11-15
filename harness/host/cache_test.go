package host

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestSiblingTemplatesSendSharedCacheKeys(t *testing.T) {
	options := make(chan llm.RequestOptions, 1)
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(ctx context.Context, r llm.Request, opt llm.RequestOptions) (llm.Response, error) {
		options <- opt
		return echo(ctx, r, opt)
	}))})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	keys := map[string]string{}
	for _, tc := range []struct{ id, parent, template string }{
		{"main", "", ""},
		{"child-a", "main", "worker-high"},
		{"child-b", "main", "worker-high"},
		{"child-c", "main", "worker-low"},
		{"child-d", "other-parent", "worker-high"},
		{"legacy-child", "main", ""},
	} {
		raw, err := json.Marshal(map[string]string{"ParentID": tc.parent, "Template": tc.template})
		if err != nil {
			t.Fatal(err)
		}
		o := Options{ID: session.ID(tc.id), Configuration: raw}
		for _, resume := range []bool{false, true} {
			if resume {
				o.Mode = Resume
			}
			s, err := h.Open(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			inputID := "first"
			if resume {
				inputID = "resumed"
			}
			if _, err := s.Submit(timeout(t), s.Generation, input(inputID, "work")); err != nil {
				t.Fatal(err)
			}
			var opt llm.RequestOptions
			select {
			case opt = <-options:
			case <-timeout(t).Done():
				t.Fatal("no model request")
			}
			if opt.Identity.SessionID != tc.id {
				t.Fatal("cache sharing changed request identity", opt.Identity)
			}
			if resume && keys[tc.id] != opt.CacheKey {
				t.Fatal("resume changed the cache key", tc.id, opt.CacheKey)
			}
			keys[tc.id] = opt.CacheKey
			stop(t, s)
		}
	}
	if keys["main"] != "main" || keys["legacy-child"] != "legacy-child" {
		t.Fatal("Session-specific defaults changed", keys)
	}
	if keys["child-a"] == "child-a" || keys["child-a"] != keys["child-b"] || keys["child-a"] == keys["child-c"] || keys["child-a"] == keys["child-d"] {
		t.Fatal("wrong sibling cache grouping", keys)
	}
}
