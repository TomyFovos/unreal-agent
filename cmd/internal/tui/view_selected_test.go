package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// The terminal starts in the saved view and reports only the views the person
// chooses, never the Chat fallback it reports to the observer on exit.
func TestViewSelectionStartsSavedAndReportsChoices(t *testing.T) {
	f := conversationFixture("SAVED VIEW RESPONSE")
	var mu sync.Mutex
	var chosen []ViewMode
	keys, out, done := make(chan Key, 32), &screenObserver{}, make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 160, 40 },
			InitialView: ViewChat,
			ViewSelected: func(v ViewMode) {
				mu.Lock()
				defer mu.Unlock()
				chosen = append(chosen, v)
			},
		})
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "SAVED VIEW RESPONSE") })
	viewCommand(keys, "/view orchestration")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(chosen) == 1
	})
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chosen) != 1 || chosen[0] != ViewOrchestration {
		t.Fatalf("reported views = %v; want only the chosen orchestration view", chosen)
	}
}
