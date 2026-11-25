// Package usage projects measured calls, tokens and charges from Session
// records. It does not estimate prices or drive execution policy.
package usage

import (
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type Sample struct {
	At                                                        time.Time
	Provider, Model                                           string
	Calls                                                     int
	Input, Uncached, CacheRead, CacheWrite, Output, Reasoning *int64
	Charge                                                    *llm.Charge
}

// Session holds only the canonical evidence needed for usage and allowance.
type Session struct {
	Selection       *sessionstore.RuntimeSelection
	Parent          session.ID
	Layer, Template string
	Latest          llm.SubscriptionObservations
	Allowance       []llm.SubscriptionObservation
	samples         []Sample
	turns           map[session.TurnID]session.TurnID
	streams         map[session.TurnID][]Sample
	seenStreams     map[string]bool
}

func (s *Session) Turn(t session.Turn) {
	if s.turns == nil {
		s.turns = make(map[session.TurnID]session.TurnID)
	}
	invocation := t.ID
	if t.ToolContinuation != "" {
		invocation = s.invocation(t.ToolContinuation)
	}
	s.turns[t.ID] = invocation
}

func (s *Session) invocation(turn session.TurnID) session.TurnID {
	if id := s.turns[turn]; id != "" {
		return id
	}
	return turn
}

func (s *Session) Host(at time.Time, h sessionstore.HostRecord) {
	switch h.Kind {
	case "configuration":
		s.Selection = sessionstore.SelectionFromConfiguration(h.Configuration)
	case sessionstore.HostRuntimeApplied:
		s.Selection = h.Selection
	case sessionstore.HostActivity:
		if h.Activity != nil {
			s.activity(at, *h.Activity)
		}
	}
}

func (s *Session) activity(at time.Time, a llm.ActivityRecord) {
	s.Latest.Observe(a, at)
	if a.Kind == llm.ActivitySubscriptionUsage {
		for _, w := range a.Subscription {
			if w.Window != "" {
				s.Allowance = append(s.Allowance, llm.SubscriptionObservation{
					Provider: a.Provider, ObservedAt: at, SubscriptionUsage: w,
				})
			}
		}
	}
	turn, ok := claudeagent.StreamCall(a)
	if !ok || s.seenStreams[a.ID] {
		return
	}
	if s.streams == nil {
		s.streams = make(map[session.TurnID][]Sample)
		s.seenStreams = make(map[string]bool)
	}
	s.seenStreams[a.ID] = true
	provider, model := s.identity(a.Provider, a.Model)
	call := Sample{At: at, Provider: provider, Model: model, Calls: 1}
	if !a.StartedAt.IsZero() {
		call.At = a.StartedAt
	}
	if u := a.RequestUsage; u != nil {
		call.Input, call.CacheRead, call.CacheWrite, call.Uncached = u.Input, u.Read, u.Write, u.Uncached
	}
	turn = s.invocation(turn)
	s.streams[turn] = append(s.streams[turn], call)
}

func (s *Session) identity(provider, model string) (string, string) {
	if s.Selection != nil {
		if provider == "" {
			provider = s.Selection.Provider
		}
		if model == "" {
			model = s.Selection.Model
		}
	}
	return provider, model
}

func (s *Session) Response(at time.Time, record sessionstore.ModelResponse) {
	record.ObserveSubscriptionSuccess(&s.Latest, s.Selection, at)
	r := record.Response
	u := r.Usage
	if u.Charge == nil {
		u.Charge = opencodego.ReportedCharge(u.Raw)
	}
	tokens := llm.ReportedTokens(u)
	if r.ID == "" && r.Provider == "" && r.Model == "" && u.Charge == nil &&
		!tokens.InputKnown && !tokens.OutputKnown {
		return // synthetic receipt or an error without evidence of a response
	}
	provider := r.Provider
	if provider == "" {
		provider = u.Provider
	}
	if provider == "" && u.Charge != nil && r.Usage.Charge == nil {
		provider = opencodego.ProviderID
	}
	provider, model := s.identity(provider, r.Model)
	call := Sample{At: at, Provider: provider, Model: model, Calls: 1, Charge: u.Charge}
	known := func(value int64, field llm.UsageField) *int64 {
		if value < 0 || slices.Contains(u.Unknown, field) {
			return nil
		}
		if !tokens.InputKnown && !tokens.OutputKnown && value == 0 {
			return nil
		}
		return new(value)
	}
	call.Input = known(u.InputTokens, llm.UsageInput)
	call.CacheRead = known(u.CachedInputTokens, llm.UsageCachedInput)
	call.CacheWrite = known(u.CacheWriteInputTokens, llm.UsageCacheWriteInput)
	if call.Input != nil && call.CacheRead != nil && call.CacheWrite != nil &&
		*call.Input >= *call.CacheRead && *call.Input-*call.CacheRead >= *call.CacheWrite {
		call.Uncached = new(*call.Input - *call.CacheRead - *call.CacheWrite)
	}
	call.Output = known(u.OutputTokens, llm.UsageOutput)
	call.Reasoning = known(u.ReasoningTokens, llm.UsageReasoning)
	invocation := s.invocation(record.TurnID)
	if pending := s.streams[invocation]; len(pending) > 0 {
		// A tool continuation may account for several Claude requests. Use
		// the response's token sums once and count its observed requests.
		call.Calls = len(pending)
		delete(s.streams, invocation)
	}
	s.samples = append(s.samples, call)
}

func (s *Session) Samples(yield func(Sample) bool) {
	for _, sample := range s.samples {
		if !yield(sample) {
			return
		}
	}
	// A request may end without a canonical model response. Its stream start
	// still measures a call and any input counters it reported.
	for _, pending := range s.streams {
		for _, sample := range pending {
			if !yield(sample) {
				return
			}
		}
	}
}
