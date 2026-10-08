package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type delayedModelSubscription struct {
	*fakeClient
	release <-chan struct{}
}

func (f *delayedModelSubscription) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	select {
	case <-f.release:
		return f.fakeClient.Subscribe(ctx, id, after, limit, capacity)
	case <-ctx.Done():
		return host.Subscription{}, ctx.Err()
	}
}

func TestModelOpenedBeforeInitialSyncRetainsCanonicalProviderNamespace(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude-code","model":{"id":"old-model"}},"ReasoningEffort":"medium"}`)}}}
	release := make(chan struct{})
	client := &delayedModelSubscription{fakeClient: f, release: release}
	keys := make(chan Key, 8)
	out := &screenObserver{}
	done := make(chan error, 1)
	refreshed := make(chan struct{}, 1)
	selected := make(chan sessionstore.RuntimeSelection, 1)
	noEffort := false
	ctx, cancel := context.WithCancel(t.Context())
	defer func() {
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Fatal(err)
		}
	}()
	go func() {
		done <- Run(ctx, Config{Client: client, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, RefreshModelCatalog: func(context.Context) (modelcatalog.Catalog, error) {
			refreshed <- struct{}{}
			return modelcatalog.Catalog{Available: true, Authoritative: true, Models: []modelcatalog.Model{{ID: "no-effort", Name: "No effort model", SupportsEffort: &noEffort}}}, nil
		}, SelectModel: func(_ context.Context, _ string, _ uint64, s sessionstore.RuntimeSelection) error {
			selected <- s
			return nil
		}})
	}()
	keys <- Key{Text: "/model refresh"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "cancel waiting") || strings.Contains(out.text(), "No effort model")
	})
	select {
	case <-refreshed:
		t.Fatal("model catalog opened before its canonical provider namespace was observed")
	default:
	}
	close(release)
	waitFor(t, func() bool { return strings.Contains(out.text(), "No effort model") })
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "No effort parameter") })
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return len(selected) == 1 })
	choice := <-selected
	if choice.Provider != "claude-code" || choice.Model != "no-effort" || choice.Effort != "" || choice.Validate() != nil {
		t.Fatal("initial sync lost the canonical provider/model/effort")
	}
}
