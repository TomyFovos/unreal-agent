package agentrunner

import (
	"context"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

// DiscoveredBackends publishes complete registrations without mutating the
// configuration maps borrowed by executing sessions.
type DiscoveredBackends struct {
	mu       sync.RWMutex
	entries  map[string]DiscoveredBackend
	statuses []modelcatalog.Provider
}
type DiscoveredBackend struct {
	Runtime ProviderRuntime
	Catalog func(context.Context, bool) (modelcatalog.Catalog, error)
	Check   func(context.Context) error
}

func (d *DiscoveredBackends) Register(b DiscoveredBackend) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = make(map[string]DiscoveredBackend)
	}
	d.entries[b.Runtime.Provider.Provider] = b
}
func (d *DiscoveredBackends) Lookup(id string) (DiscoveredBackend, bool) {
	if d == nil {
		return DiscoveredBackend{}, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	b, ok := d.entries[id]
	return b, ok
}
func (d *DiscoveredBackends) IDs() []string {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	ids := make([]string, 0, len(d.entries))
	for id := range d.entries {
		ids = append(ids, id)
	}
	return ids
}

func (d *DiscoveredBackends) SetStatuses(statuses []modelcatalog.Provider) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statuses = append([]modelcatalog.Provider(nil), statuses...)
}
func (d *DiscoveredBackends) Statuses() []modelcatalog.Provider {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]modelcatalog.Provider(nil), d.statuses...)
}
