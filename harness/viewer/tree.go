package viewer

import (
	"context"
	"fmt"
	"sort"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// InspectTree is an explicit analysis read. Ordinary child controls retain
// their direct-parent scope. Every edge is proven by both canonical records.
func (r ParentReader) InspectTree(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if limit < 1 || limit > 4096 {
		return host.View{}, ErrInvalidPage
	}
	if r.cache == nil {
		r.cache = newParentReadCache()
	}
	if _, err := r.owner(ctx); err != nil {
		return host.View{}, err
	}
	return r.inspectTree(ctx, id, after, min(limit, 256), map[session.ID]bool{})
}

func (r ParentReader) inspectTree(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int, seen map[session.ID]bool) (host.View, error) {
	if err := ctx.Err(); err != nil {
		return host.View{}, err
	}
	if seen[id] {
		return host.View{}, fmt.Errorf("cyclic child lineage")
	}
	seen[id] = true
	child, err := r.cache.treeChild(ctx, r.Directory, id)
	if err != nil {
		return host.View{}, err
	}
	if id != r.ParentID {
		config := child.Configuration
		if config == nil || config.ChildID != id {
			return host.View{}, ErrUnavailable
		}
		parent, err := r.inspectTree(ctx, config.ParentID, 0, 1, seen)
		if err != nil {
			return host.View{}, err
		}
		matches := 0
		for _, op := range parent.Operations {
			c, ok, err := DecodeSubagent(config.ParentID, op)
			if err != nil {
				return host.View{}, err
			}
			if ok && c.ID == id && op.ID == config.OperationID {
				matches++
			}
		}
		if matches != 1 {
			return host.View{}, ErrUnavailable
		}
	}
	v := child.View
	items := v.History.Items
	start := min(uint64(after), uint64(len(items)))
	end := min(start+uint64(limit), uint64(len(items)))
	next := after
	if end > start {
		next = items[end-1].Sequence
	}
	v.History = host.HistoryPage{Items: items[start:end], NextAfter: next, More: end < uint64(len(items))}
	if f := v.ContextFacts; f != nil {
		lo := 0
		if after > 0 {
			lo = sort.Search(len(f.Rows), func(i int) bool { return f.Rows[i].After > after })
		}
		hi := sort.Search(len(f.Rows), func(i int) bool { return f.Rows[i].After > next })
		v.ContextFacts = &sessionstore.ContextFacts{Through: next, Rows: f.Rows[lo:hi]}
	}
	return copyValue(v)
}

func (r Remote) InspectTree(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if id == r.ParentID && r.Call == nil {
		return r.Inspect(ctx, id, after, limit)
	}
	reply, err := r.exchange(ctx, extensionRequest{Action: "inspect_tree", ParentID: r.ParentID, ID: id, After: after, Limit: limit})
	if err != nil {
		return host.View{}, err
	}
	if reply.View == nil || reply.View.Session.Session.ID != id {
		return host.View{}, &ExtensionError{Code: "invalid_reply"}
	}
	return *reply.View, nil
}
