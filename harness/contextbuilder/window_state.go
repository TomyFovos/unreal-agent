package contextbuilder

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (b *builder) windowState(ctx context.Context, recentEventCount int) (contextstate.State, sessionstore.Sequence, error) {
	var state contextstate.State
	var seq sessionstore.Sequence
	if b.windowSource.state != nil {
		var err error
		state, seq, err = b.windowSource.state(ctx)
		if err != nil {
			return contextstate.State{}, 0, err
		}
	}
	state.Session.ID = string(b.windowSource.session)
	state.Session.Provider, state.Session.Model = b.provider, b.request.Model.ID
	state.Session.Effort = string(b.request.Model.ReasoningEffort)
	state.Requests.Current = maps.Clone(state.Requests.Current)
	if b.windowTurn.request != b.lastHumanInput {
		b.fillWindowRequest(&state, b.windowTurn.request)
	}
	b.fillWindowRequest(&state, b.lastHumanInput)
	for id, request := range state.Requests.Current {
		if request.Status == "" && id != string(b.lastHumanInput.id) && id != string(b.windowTurn.request.id) {
			delete(state.Requests.Current, id)
		}
	}
	return state.Render(recentEventCount), seq, nil
}

func (b *builder) fitWindowState(state contextstate.State, heading string, target int64) (llm.Item, int, error) {
	render := func(state contextstate.State, heading string) (llm.Item, bool, error) {
		raw, err := state.MarshalJSON()
		if err != nil {
			return llm.Item{}, false, err
		}
		item, _ := contextengine.PublicItem(windowStateItem(heading + "\n" + string(raw)))
		return item, b.windowStateTokens(item) <= target, nil
	}
	full, fits, err := render(state, heading)
	if err != nil || fits || b.windowSource.session == "" {
		return full, len(state.RecentEvents), err
	}

	index := record.Address{Kind: record.ContextIndex, Session: string(b.windowSource.session), Section: "state"}.Locator()
	heading += fmt.Sprintf(" More recorded state is in L3. Use record find for specific evidence or page through record show %s.", index)
	if _, envelopeFits, err := render(contextstate.State{}, heading); err != nil || !envelopeFits {
		return full, len(state.RecentEvents), err
	}
	projected := state
	sections := windowStateSections(&projected)
	item, fits, err := render(projected, heading)
	if err != nil {
		return llm.Item{}, 0, err
	}
	if !fits {
		// An unaddressable entry or the state envelope itself can exceed the
		// target. Keep that minimum and still displace addressable entries.
		if b.windowStateTokens(item) >= b.windowStateTokens(full) {
			return full, len(state.RecentEvents), nil
		}
		return item, len(projected.RecentEvents), nil
	}
	for _, section := range sections {
		// Keep the highest-priority prefix of this section that fits the
		// actual rendered L2, including its L3 locator and JSON framing.
		lo, hi := 0, section.count+1
		for hi-lo > 1 {
			n := lo + (hi-lo)/2
			section.selectPrefix(n)
			candidate, fits, err := render(projected, heading)
			if err != nil {
				return llm.Item{}, 0, err
			}
			if fits {
				lo, item = n, candidate
			} else {
				hi = n
			}
		}
		section.selectPrefix(lo)
	}
	return item, len(projected.RecentEvents), nil
}

type windowStateSection struct {
	count        int
	selectPrefix func(int)
}

// Sections with live work are filled before history. Within each map, newer
// records take precedence, with stable ordering for equal record positions.
func windowStateSections(s *contextstate.State) []windowStateSection {
	return []windowStateSection{
		windowStateMap(&s.Requests.Current, func(v contextstate.Request) stateOrder {
			order := stateEvidence(v.Locator, v.Sources)
			order.live = v.Status == contextstate.RequestActive
			return order
		}),
		windowStateMap(&s.Approvals.Current, func(v contextstate.Approval) stateOrder { return stateEvidence(v.Locator, v.Sources) }),
		windowStateMap(&s.QuestionsToUser.Current, func(v contextstate.Question) stateOrder { return stateEvidence(v.Locator, v.Sources) }),
		windowStateMap(&s.Waiting.Current, func(v contextstate.Wait) stateOrder { return stateEvidence(v.Question, v.Sources) }),
		windowStateMap(&s.Children.Current, func(v contextstate.Child) stateOrder {
			order := stateEvidence(v.LastEvent, v.Sources)
			order.live = v.State == contextstate.ChildRunning || v.State == contextstate.ChildWaitingApproval ||
				v.State == contextstate.ChildAsked || len(v.UnansweredQuestions) > 0
			return order
		}),
		windowStateSlice(&s.Plan.Steps, false, func(v contextstate.PlanStep) bool { return stateEvidence("", v.Sources).indexed }),
		windowStateMap(&s.Issues.Current, func(v contextstate.Issue) stateOrder { return stateEvidence(v.Locator, v.Sources) }),
		windowStateMap(&s.Rules, func(v contextstate.Rule) stateOrder { return stateEvidence(v.Locator, v.Sources) }),
		windowStateMap(&s.Decisions, func(v contextstate.Decision) stateOrder { return stateEvidence(v.Locator, v.Sources) }),
		windowStateMap(&s.Artifacts.Current, func(v contextstate.Artifact) stateOrder { return stateEvidence(v.Locator, v.Sources) }),
		windowStateSlice(&s.RecentEvents, true, func(v contextstate.RecentEvent) bool { return stateEvidence(v.Locator, v.Sources).indexed }),
		windowStateSlice(&s.Children.ByEnding, false, func(contextstate.ChildCount) bool { return true }),
	}
}

type stateOrder struct {
	sequence      uint64
	indexed, live bool
}

func stateEvidence(primary record.Locator, sources []record.Locator) stateOrder {
	var order stateOrder
	add := func(loc record.Locator) {
		if loc == "" {
			return
		}
		if address, err := loc.Address(); err == nil {
			order.indexed = true
			order.sequence = max(order.sequence, address.Sequence)
		}
	}
	add(primary)
	for _, loc := range sources {
		add(loc)
	}
	return order
}

func windowStateMap[T any](dest *map[string]T, order func(T) stateOrder) windowStateSection {
	original := *dest
	type entry struct {
		key   string
		order stateOrder
	}
	var indexed []entry
	var required map[string]T
	for key, value := range original {
		o := order(value)
		if o.indexed {
			indexed = append(indexed, entry{key, o})
		} else {
			if required == nil {
				required = make(map[string]T)
			}
			required[key] = value
		}
	}
	slices.SortFunc(indexed, func(a, b entry) int {
		if a.order.live != b.order.live {
			if a.order.live {
				return -1
			}
			return 1
		}
		if a.order.sequence != b.order.sequence {
			return cmp.Compare(b.order.sequence, a.order.sequence)
		}
		return cmp.Compare(a.key, b.key)
	})
	*dest = required
	return windowStateSection{len(indexed), func(n int) {
		*dest = maps.Clone(required)
		if n > 0 && *dest == nil {
			*dest = make(map[string]T, n)
		}
		for _, e := range indexed[:n] {
			(*dest)[e.key] = original[e.key]
		}
	}}
}

func windowStateSlice[T any](dest *[]T, newestFirst bool, indexed func(T) bool) windowStateSection {
	original := *dest
	var optional, required []int
	for i, v := range original {
		if indexed(v) {
			optional = append(optional, i)
		} else {
			required = append(required, i)
		}
	}
	if newestFirst {
		slices.Reverse(optional)
	}
	selectPrefix := func(n int) {
		kept := append(slices.Clone(required), optional[:n]...)
		slices.Sort(kept)
		*dest = nil
		for _, i := range kept {
			*dest = append(*dest, original[i])
		}
	}
	selectPrefix(0)
	return windowStateSection{len(optional), selectPrefix}
}
