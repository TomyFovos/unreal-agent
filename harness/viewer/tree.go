package viewer

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// InspectTree is an explicit analysis read. Ordinary child controls retain
// their direct-parent scope. Every edge is proven by both canonical records.
func (r ParentReader) InspectTree(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	return r.inspectTree(ctx, id, after, limit, true)
}

// InspectTopology observes the visible tree using the same lineage proof as
// analysis. The direct-child reader remains the fast path for direct children.
func (r ParentReader) InspectTopology(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if v, err := r.Inspect(ctx, id, after, limit); !errors.Is(err, ErrUnavailable) {
		if err != nil || id == r.ParentID {
			return v, err
		}
		owner, err := r.owner(ctx)
		if err != nil {
			return host.View{}, err
		}
		v.ModelPhase = owner.ChildModelPhase([]session.ID{id})
		return v, nil
	}
	return r.inspectTree(ctx, id, after, limit, false)
}

func (r ParentReader) inspectTree(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int, analysis bool) (host.View, error) {
	if limit < 1 || limit > 4096 {
		return host.View{}, ErrInvalidPage
	}
	if r.cache == nil {
		r.cache = newParentReadCache()
	}
	owner, err := r.owner(ctx)
	if err != nil {
		return host.View{}, err
	}
	child, path, err := r.treeChild(ctx, owner, id, analysis, map[session.ID]bool{})
	if err != nil {
		return host.View{}, err
	}
	v := childPage(child.View, after, min(limit, 256), analysis)
	if !analysis {
		v.ModelPhase = owner.ChildModelPhase(path)
	}
	if f := v.ContextFacts; f != nil {
		lo := sort.Search(len(f.Rows), func(i int) bool { return f.Rows[i].After > after })
		hi := sort.Search(len(f.Rows), func(i int) bool { return f.Rows[i].After > v.History.NextAfter })
		v.ContextFacts = &sessionstore.ContextFacts{Through: v.History.NextAfter, Rows: f.Rows[lo:hi]}
	}
	return copyValue(v)
}

func (r ParentReader) treeChild(ctx context.Context, owner *host.Session, id session.ID, analysis bool, seen map[session.ID]bool) (subagent.ChildView, []session.ID, error) {
	if err := ctx.Err(); err != nil {
		return subagent.ChildView{}, nil, err
	}
	if seen[id] {
		return subagent.ChildView{}, nil, fmt.Errorf("cyclic child lineage")
	}
	seen[id] = true
	mode := childReadHistory
	if analysis {
		mode = childReadAnalysis
	}
	child, err := r.cache.treeChild(ctx, r.Directory, id, mode)
	if err != nil {
		return subagent.ChildView{}, nil, err
	}
	if id == r.ParentID {
		return child, nil, nil
	}
	config := child.Configuration
	if config == nil || config.ChildID != id {
		return subagent.ChildView{}, nil, ErrUnavailable
	}
	if config.ParentID == owner.ID {
		r.cache.mu.Lock()
		op, err := r.cache.childOperation(owner, id)
		r.cache.mu.Unlock()
		if err != nil {
			return subagent.ChildView{}, nil, err
		}
		if op != config.OperationID {
			return subagent.ChildView{}, nil, ErrUnavailable
		}
		return child, []session.ID{id}, nil
	}
	parent, parentPath, err := r.treeChild(ctx, owner, config.ParentID, false, seen)
	if err != nil {
		return subagent.ChildView{}, nil, err
	}
	matches := 0
	for _, op := range parent.View.Operations {
		c, ok, err := DecodeSubagent(config.ParentID, op)
		if err != nil {
			return subagent.ChildView{}, nil, err
		}
		if ok && c.ID == id && op.ID == config.OperationID {
			matches++
		}
	}
	if matches != 1 {
		return subagent.ChildView{}, nil, ErrUnavailable
	}
	return child, append(parentPath, id), nil
}

func (r Remote) InspectTree(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if id == r.ParentID && r.Call == nil {
		return r.Inspect(ctx, id, after, limit)
	}
	return r.inspectDescendant(ctx, "inspect_tree", id, after, limit)
}

func (r Remote) InspectTopology(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	v, err := r.inspectDescendant(ctx, "inspect_topology", id, after, limit)
	var extension *ExtensionError
	if errors.As(err, &extension) && extension.Code == "unsupported_action" {
		return r.Inspect(ctx, id, after, limit)
	}
	return v, err
}

func (r Remote) inspectDescendant(ctx context.Context, action string, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	reply, err := r.exchange(ctx, extensionRequest{Action: action, ParentID: r.ParentID, ID: id, After: after, Limit: limit})
	if err != nil {
		return host.View{}, err
	}
	if reply.View == nil || reply.View.Session.Session.ID != id {
		return host.View{}, &ExtensionError{Code: "invalid_reply"}
	}
	return *reply.View, nil
}
