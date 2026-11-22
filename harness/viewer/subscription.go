package viewer

import (
	"context"
	"reflect"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// DescendantSubscriptionUsage follows canonical starts and verifies each edge
// against the child's configuration, including children in the archive.
func (r ParentReader) DescendantSubscriptionUsage(ctx context.Context) (llm.SubscriptionObservations, error) {
	owner, err := r.owner(ctx)
	if err != nil {
		return nil, err
	}
	root, err := owner.Inspect(0, 1)
	if err != nil {
		return nil, err
	}
	if r.cache == nil {
		r.cache = newParentReadCache()
	}
	type node struct {
		id         session.ID
		operations []operation.Operation
	}
	queue := []node{{owner.ID, root.Operations}}
	seen := map[session.ID]bool{owner.ID: true}
	var latest llm.SubscriptionObservations
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, op := range n.operations {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			plan, err := subagent.DecodePlan(op)
			if err != nil || plan.Action != "start" || plan.ParentID != n.id || seen[plan.ChildID] {
				continue
			}
			child, err := r.cache.treeChild(ctx, r.Directory, plan.ChildID, childReadSubscription)
			if err != nil {
				continue
			}
			config := child.Configuration
			if config == nil || config.ParentID != n.id || config.OperationID != op.ID || config.ChildID != plan.ChildID {
				continue
			}
			seen[plan.ChildID] = true
			latest.Merge(child.SubscriptionUsage)
			queue = append(queue, node{plan.ChildID, child.View.Operations})
		}
	}
	return latest.Clone(), ctx.Err()
}

func (r Remote) DescendantSubscriptionUsage(ctx context.Context) (llm.SubscriptionObservations, error) {
	reply, err := r.exchange(ctx, extensionRequest{Action: "subscription_usage", ParentID: r.ParentID})
	return reply.SubscriptionUsage, err
}

func (p *Panel) refreshSubscriptionUsage() bool {
	r, ok := p.client.Reader.(interface {
		DescendantSubscriptionUsage(context.Context) (llm.SubscriptionObservations, error)
	})
	if !ok {
		return false
	}
	if !p.client.Model.hasChildren(p.parent) {
		p.setSubscriptionUsage(nil)
		return false
	}
	usage, err := r.DescendantSubscriptionUsage(p.ctx)
	if err == nil {
		p.setSubscriptionUsage(usage)
	}
	return true
}

func (p *Panel) setSubscriptionUsage(usage llm.SubscriptionObservations) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !reflect.DeepEqual(p.subscriptionUsage, usage) {
		p.subscriptionUsage = usage
		p.snapshotVersion++
		p.notify()
	}
}
