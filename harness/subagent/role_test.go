package subagent

import "testing"

func TestResolveLayer(t *testing.T) {
	for _, parent := range []string{"", "main", "unknown", "lead", "worker", "collector"} {
		for _, tc := range []struct{ template, main, belowLead string }{
			{"lead", "lead", "lead"},
			{"worker", "lead", "worker"},
			{"worker-low", "lead", "worker"},
			{"worker-max", "lead", "worker"},
			{"collector", "lead", "collector"},
			{"custom", "unknown", "unknown"},
		} {
			want := tc.main
			if parent == "lead" || parent == "worker" || parent == "collector" {
				want = tc.belowLead
			}
			if got := ResolveLayer(tc.template, parent); got != want {
				t.Errorf("parent %q / template %q: layer = %q; want %q", parent, tc.template, got, want)
			}
		}
	}
	layer := ResolveLayer("worker-max", "main")
	for range 8 {
		layer = ResolveLayer("worker-max", layer)
		if layer != "worker" || ResolveLayer("collector", layer) != "collector" {
			t.Fatal("lost lead ancestry below a worker", layer)
		}
	}
}
