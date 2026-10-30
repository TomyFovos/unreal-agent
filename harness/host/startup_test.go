package host

import (
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestHostStartupRecordsCreateAndResume(t *testing.T) {
	dir := t.TempDir()
	h := newTestHost(t, dir)
	s, err := h.Create(t.Context(), Options{ID: "startup"})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		v, err := s.Inspect(0, 256)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, item := range v.History.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Activity != nil && r.Activity.Kind == "host_start" {
				count++
				if r.Activity.StartedAt.IsZero() || r.Activity.Revision == "" {
					t.Fatal("startup metadata missing", r.Activity)
				}
			}
		}
		if count != want {
			t.Fatalf("host starts=%d want=%d", count, want)
		}
	}
	check(1)
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	h = newTestHost(t, dir)
	s, err = h.Resume(t.Context(), Options{ID: "startup"})
	if err != nil {
		t.Fatal(err)
	}
	check(2)
}
