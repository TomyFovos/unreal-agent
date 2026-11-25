package subagent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/usage"
)

type callTally struct {
	count        int
	providers    map[string]int
	selfNotified bool
}

func (r *UsageReader) tally(ctx context.Context, id session.ID, warn inbox.ID) (callTally, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tree, err := r.readTree(ctx, id, false)
	if err != nil {
		return callTally{}, err
	}
	tally := callTally{providers: make(map[string]int), selfNotified: tree[0].node.notices[warn]}
	for _, entry := range tree {
		if entry.node == nil {
			continue
		}
		for sample := range entry.node.Samples {
			tally.count += sample.Calls
			tally.providers[sample.Provider] += sample.Calls
		}
	}
	return tally, nil
}

func budgetPeer(kind string, sender, target session.ID, handle operation.ID, id inbox.ID, text string) inbox.Input {
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(sender), Target: string(target),
		Handle: string(handle), Kind: kind, Text: text})
	return inbox.Input{ID: id, Kind: inbox.InputPeer, Payload: payload}
}

func budgetWarningID(id operation.ID) inbox.ID {
	return inbox.ID("budget-notice:" + string(id) + ":80")
}

func budgetWarning(child, parent session.ID, id operation.ID, budget int) inbox.Input {
	return budgetPeer("progress", parent, child, id, budgetWarningID(id),
		fmt.Sprintf("Your subtree reached 80%% of its %d model-call budget. Tell your parent before exceeding it.", budget))
}

// ChildBudgetInput runs after a response commits, before the child's next
// request. A parent also checks descendants while the child is idle.
func ChildBudgetInput(owner *host.Session, c ChildConfig) func(context.Context) *inbox.Input {
	if owner == nil || c.Brief == nil || c.Brief.Fields.BudgetSteps <= 0 {
		return nil
	}
	reader, err := OpenUsageReader(c.SessionDirectory)
	if err != nil {
		return nil
	}
	budget := c.Brief.Fields.BudgetSteps
	input := budgetWarning(c.ChildID, c.ParentID, c.OperationID, budget)
	return func(ctx context.Context) *inbox.Input {
		if owner.HasCommittedInput(input.ID) {
			return nil
		}
		tally, err := reader.tally(ctx, c.ChildID, input.ID)
		if err == nil && tally.count >= budget-budget/5 {
			return &input
		}
		return nil
	}
}

func parentNoticeThresholds(calls, budget int, notify func(string)) {
	if budget == 0 {
		for threshold := 250; threshold <= calls; threshold *= 2 {
			notify(fmt.Sprintf("milestone:%d", threshold))
			if threshold > calls/2 {
				break
			}
		}
		return
	}
	for half, threshold := 2, budget; threshold <= calls; half++ {
		notify(fmt.Sprintf("budget:%d", half))
		increment := budget / 2
		if half%2 == 0 {
			increment += budget % 2
		}
		if increment > calls-threshold {
			break
		}
		threshold += increment
	}
}

func (m *Manager) checkChildUsage(ctx context.Context, p Plan, id operation.ID, channel *processChannel) {
	if m.usage == nil || ctx.Err() != nil {
		return
	}
	budget := p.Brief.BudgetSteps
	tally, err := m.usage.tally(ctx, p.ChildID, budgetWarningID(id))
	if err != nil {
		return
	}
	if channel != nil && budget > 0 && tally.count >= budget-budget/5 && !tally.selfNotified {
		// No live route during startup/after exit. The child's own boundary
		// covers a fast response and any former executor's committed notice.
		select {
		case <-channel.connected:
			_, _ = channel.send(ctx, budgetWarning(p.ChildID, p.ParentID, id, budget))
		default:
		}
	}
	var providers []string
	for _, name := range slices.Sorted(maps.Keys(tally.providers)) {
		providers = append(providers, fmt.Sprintf("%s %d", usage.Label(name), tally.providers[name]))
	}
	parentNoticeThresholds(tally.count, budget, func(threshold string) {
		noticeID := inbox.ID("budget-notice:" + string(id) + ":" + threshold)
		if m.config.Owner.HasCommittedInput(noticeID) {
			return
		}
		title := childUsageTitle(p)
		detail := strings.Join(providers, ", ")
		kind, text := "progress", fmt.Sprintf("Child %s reached %s model calls (%d so far, %s).", title, strings.TrimPrefix(threshold, "milestone:"), tally.count, detail)
		if budget > 0 {
			kind = "budget"
			text = fmt.Sprintf("Child %s used %d of %d budgeted model calls (%s). Continue, stop, or split?", title, tally.count, budget, detail)
		}
		input := budgetPeer(kind, p.ChildID, p.ParentID, id, noticeID, text)
		_, _ = m.config.Owner.SubmitPeer(ctx, m.config.Owner.Generation, string(p.ChildID), string(id), input)
	})
}

// RecoverUsageNotices catches a threshold committed just before a Host died.
// Active children are observed by their jobs. Canonical inbox IDs deduplicate
// delivery against the preceding executor and its terminal receipts.
func (m *Manager) RecoverUsageNotices() {
	if m.usage == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.workers.Go(func() {
		r := m.usage
		r.mu.Lock()
		n, err := r.readNode(m.ctx, m.config.Owner.ID)
		var starts map[operation.ID]usageChild
		if err == nil {
			starts = maps.Clone(n.children)
		}
		r.mu.Unlock()
		for _, id := range slices.Sorted(maps.Keys(starts)) {
			c := starts[id]
			if terminalStatus(c.status) && (c.plan.UsageReceipt || c.plan.Brief.BudgetSteps > 0) {
				m.checkChildUsage(m.ctx, c.plan, id, nil)
			}
		}
	})
}
