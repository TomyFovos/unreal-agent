package viewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

type parentIdentity struct {
	owner     *host.Session
	revision  uint64
	children  map[session.ID]operation.ID
	ambiguous map[session.ID]bool
	err       error
}

type childObservation struct {
	info  os.FileInfo
	child subagent.ChildView
}

// This cache belongs to the serving Host, never to a Session or its writer.
// Every request still checks the owning parent and the child's configuration.
type parentReadCache struct {
	mu       sync.Mutex
	parents  map[session.ID]parentIdentity
	children map[session.ID]childObservation
}

func newParentReadCache() *parentReadCache {
	return &parentReadCache{parents: make(map[session.ID]parentIdentity), children: make(map[session.ID]childObservation)}
}

func (c *parentReadCache) inspect(ctx context.Context, owner *host.Session, parent session.ID, directory string, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if err := context.Cause(ctx); err != nil {
		return host.View{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	revision := owner.OperationRevision()
	p, ok := c.parents[parent]
	if !ok || p.owner != owner || p.revision != revision {
		latest, err := owner.Inspect(0, 1)
		if err != nil {
			return host.View{}, err
		}
		p = parentIdentity{owner: owner, revision: revision, children: make(map[session.ID]operation.ID), ambiguous: make(map[session.ID]bool)}
		for _, op := range latest.Operations {
			child, ok, err := DecodeSubagent(parent, op)
			if err != nil {
				p.err = err
				break
			}
			if !ok {
				continue
			}
			if p.children[child.ID] != "" {
				p.ambiguous[child.ID] = true
			}
			p.children[child.ID] = op.ID
		}
		c.parents[parent] = p
	}
	if p.err != nil {
		return host.View{}, p.err
	}
	if p.ambiguous[id] {
		return host.View{}, fmt.Errorf("ambiguous child identity")
	}
	identity := p.children[id]
	if identity == "" {
		return host.View{}, ErrUnavailable
	}
	// id comes from the validated canonical Child plan above, not from an
	// arbitrary path supplied by the gateway caller.
	info, err := os.Stat(filepath.Join(directory, string(id)+".session.jsonl"))
	if err != nil {
		delete(c.children, id)
		return host.View{}, err
	}
	observed, ok := c.children[id]
	if !ok || !os.SameFile(info, observed.info) || info.Size() != observed.info.Size() || !info.ModTime().Equal(observed.info.ModTime()) {
		child, err := subagent.ReadChildSnapshot(ctx, directory, id)
		if err != nil {
			delete(c.children, id)
			return host.View{}, err
		}
		observed = childObservation{info: info, child: child}
		c.children[id] = observed
	}
	config := observed.child.Configuration
	if config == nil || config.ParentID != parent || config.ChildID != id || config.OperationID != identity {
		return host.View{}, fmt.Errorf("child canonical configuration does not match parent")
	}
	view := observed.child.View
	items := view.History.Items
	start := min(uint64(after), uint64(len(items)))
	end := min(start+uint64(limit), uint64(len(items)))
	next := after
	if end > start {
		next = items[end-1].Sequence
	}
	view.History = host.HistoryPage{Items: items[start:end], NextAfter: next, More: end < uint64(len(items))}
	// Protect the cached observation from callers mutating public snapshots.
	return copyValue(view)
}
