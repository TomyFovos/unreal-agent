package analysis

import (
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// Metric values use seconds, tokens/second, or USD as named by the field.
// Unknown observations never contribute a measured zero. A value with Unknown
// observations is a partial sum (or an average of only its known samples).
type Metric struct {
	Known                      bool
	Value                      float64
	Measured, Derived, Unknown int
}

func (m *Metric) add(value float64, source string) {
	m.Known = true
	m.Value += value
	if source == "measured" {
		m.Measured++
	} else {
		m.Derived++
	}
}

func (m *Metric) merge(other Metric) {
	m.Known = m.Known || other.Known
	m.Value += other.Value
	m.Measured += other.Measured
	m.Derived += other.Derived
	m.Unknown += other.Unknown
}

func (m Metric) average() Metric {
	if n := m.Measured + m.Derived; n > 0 {
		m.Value /= float64(n)
	}
	return m
}

type ModelKey struct{ Provider, Model, Effort string }

func modelKey(s *sessionstore.RuntimeSelection) ModelKey {
	if s == nil {
		return ModelKey{}
	}
	return ModelKey{viewer.SafeText(s.Provider), viewer.SafeText(s.Model), viewer.SafeText(string(s.Effort))}
}

type RequestMetric struct {
	ModelKey
	SelectionSource string
	Fast            string `json:",omitzero"`
	Kind            string // response or compaction
	At              time.Time
	Usage           viewer.Usage
	Duration        Metric
	OutputTPS       Metric
	ListPriceUSD    Metric
	// TimedOutput is only the tokens paired with Duration, never all output
	// divided by the subset of responses that happened to have timing.
	TimedOutput int64
}

type childStart struct {
	activity   llm.ActivityRecord
	recordedAt time.Time
}
type reviewEvidence struct {
	decision   string
	rejections int
}
type operationTiming struct{ start, end time.Time }
type sessionMetrics struct {
	id              session.ID
	created         time.Time
	parent          session.ID
	template        string
	layer           string
	selection       *sessionstore.RuntimeSelection
	requests        []RequestMetric
	starts          map[string]childStart
	reviews         map[string]reviewEvidence
	times           map[operation.ID]operationTiming
	finish          string
	ended           time.Time
	endSource       string
	wait            Metric
	waitStart       time.Time
	waitSource      string
	responseRuntime llm.ActivityRecord
}

func newSessionMetrics(id session.ID) *sessionMetrics {
	return &sessionMetrics{id: id, starts: map[string]childStart{}, reviews: map[string]reviewEvidence{}, times: map[operation.ID]operationTiming{}}
}

func (m *sessionMetrics) apply(item host.HistoryItem, active *sessionstore.RuntimeSelection) {
	switch v := item.Data.(type) {
	case sessionstore.HostRecord:
		if v.Kind == sessionstore.HostContext && v.Context != nil {
			c := v.Context
			if c.Usage != nil {
				m.addRequest(*c.Usage, nil, modelKey(active), item.RecordedAt, "compaction")
			}
			if c.Conversation != nil {
				conversation := c.Conversation
				if conversation.Usage != nil && (conversation.Phase == "compact" || conversation.Compacted || conversation.Interrupted || conversation.Error != "") {
					key := modelKey(active)
					if conversation.Provider != "" {
						key.Provider = viewer.SafeText(conversation.Provider)
					}
					var generation *llm.GenerationStatistics
					if !conversation.StartedAt.IsZero() && conversation.EndedAt.After(conversation.StartedAt) {
						generation = &llm.GenerationStatistics{Tokens: llm.ReportedTokens(*conversation.Usage), DurationMicros: conversation.EndedAt.Sub(conversation.StartedAt).Microseconds(), Source: "client_clock"}
					}
					m.addRequest(*conversation.Usage, generation, key, item.RecordedAt, "compaction")
				}
			}
		}
		if v.Kind == "configuration" {
			var config struct {
				ParentID session.ID
				Template string
				Layer    string
			}
			if json.Unmarshal(v.Configuration, &config) == nil {
				m.parent, m.template = config.ParentID, config.Template
				m.layer = config.Layer
			}
			if m.selection == nil {
				m.selection = sessionstore.SelectionFromConfiguration(v.Configuration)
			}
		}
		if v.Kind == "finish" && v.Finish != nil {
			m.finish, m.ended = v.Finish.Result.Status, item.RecordedAt
			m.endSource = "measured"
		}
		if v.Kind == sessionstore.HostEnded && v.End != nil {
			m.ended = v.End.EndedAt
			m.endSource = "derived" // last known executor activity, not observed completion
		}
		if v.Kind == sessionstore.HostChildReview && v.ChildReview != nil {
			r := m.reviews[v.ChildReview.Handle]
			r.decision = v.ChildReview.Decision
			if r.decision == "reject" {
				r.rejections++
			}
			m.reviews[v.ChildReview.Handle] = r
		}
		if a := v.Activity; a != nil {
			// A child's own request activity can identify its transport. Starts
			// of its descendants describe those children, never this Session.
			if a.Kind != "child_start" && a.Provider != "" {
				m.responseRuntime = *a
			}
			if a.Kind == "host_start" && m.finish == "" {
				m.ended = time.Time{}
				m.endSource = ""
			}
			if a.Kind == "child_start" {
				// Restarts may record another launch; retain the first start.
				if _, ok := m.starts[a.ID]; !ok {
					m.starts[a.ID] = childStart{*a, item.RecordedAt}
				}
			}
			if a.Kind == "usage_limit_wait" {
				at, source := a.StartedAt, "measured"
				if at.IsZero() {
					at, source = item.RecordedAt, "derived"
				}
				if !a.EndedAt.IsZero() {
					m.addWait(at, a.EndedAt, source)
					m.waitStart = time.Time{}
				} else if a.RetryAt.IsZero() {
					if !m.waitStart.IsZero() {
						if m.waitSource == "derived" {
							source = "derived"
						}
						m.addWait(m.waitStart, at, source)
					}
					m.waitStart = time.Time{}
				} else if m.waitStart.IsZero() {
					m.waitStart, m.waitSource = at, source
				}
			}
		}
	case sessionstore.ModelResponse:
		key, source := m.responseModel(v.Response, active)
		m.addRequest(v.Response.Usage, v.Response.Generation, key, item.RecordedAt, "response")
		m.requests[len(m.requests)-1].SelectionSource = source
	case sessionstore.ToolCallStatus:
		for _, op := range v.Operations {
			t := m.times[op.ID]
			if t.start.IsZero() {
				t.start = item.RecordedAt
			}
			if op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled {
				t.end = item.RecordedAt
			}
			m.times[op.ID] = t
		}
	case sessionstore.Fork:
		m.parent, m.template, m.finish, m.endSource = "", "", "", ""
		m.layer = ""
		m.selection = nil
		m.responseRuntime = llm.ActivityRecord{}
		m.ended = time.Time{}
	}
}

func (m *sessionMetrics) responseModel(response llm.Response, active *sessionstore.RuntimeSelection) (ModelKey, string) {
	key := modelKey(active)
	if response.Model == "" {
		return key, "derived"
	}
	key.Model = viewer.SafeText(response.Model)
	key.Provider = viewer.SafeText(response.Provider)
	if key.Provider == "" {
		key.Provider = viewer.SafeText(response.Usage.Provider)
	}
	a := m.responseRuntime
	activityMatches := a.Model == response.Model || a.ID != "" && a.ID == response.ID
	if key.Provider == "" && activityMatches {
		key.Provider = viewer.SafeText(a.Provider)
	}
	if key.Provider == "" && active != nil && response.Model == active.Model {
		key.Provider = viewer.SafeText(active.Provider)
	}
	// AuthMethod identifies how a request authenticated, not its provider.
	// Use exact identities from the selectable catalog, never model prefixes.
	if key.Provider == "" && slices.Contains(opencodego.OfferedModels(), response.Model) {
		key.Provider = opencodego.ProviderID
	}
	key.Effort = ""
	if activityMatches && a.Provider == key.Provider {
		key.Effort = viewer.SafeText(a.Effort)
	} else if active != nil && response.Model == active.Model && key.Provider == active.Provider {
		key.Effort = viewer.SafeText(string(active.Effort))
	}
	return key, "derived-from-responses"
}

func (m *sessionMetrics) addRequest(usage llm.Usage, g *llm.GenerationStatistics, model ModelKey, at time.Time, kind string) {
	r := RequestMetric{ModelKey: model, At: at, Kind: kind}
	if usage.Fast != nil {
		r.Fast = "off"
		if *usage.Fast {
			r.Fast = "on"
		}
	}
	viewer.AddUsage(&r.Usage, usage)
	if g != nil && g.Valid() {
		source := "measured"
		if g.Source == "client_clock" {
			source = "derived"
		}
		r.Duration.add(float64(g.DurationMicros)/1e6, source)
		if rate, ok := g.OutputTPS(); ok {
			r.OutputTPS.add(rate, source)
			r.TimedOutput = g.Tokens.Output
		} else {
			r.OutputTPS.Unknown++
		}
	} else {
		r.Duration.Unknown++
		r.OutputTPS.Unknown++
	}
	if cost, ok := listPrice(model, usage, at); ok {
		r.ListPriceUSD.add(cost, "derived")
	} else {
		r.ListPriceUSD.Unknown++
	}
	m.requests = append(m.requests, r)
}

func (m *sessionMetrics) addWait(start, end time.Time, source string) {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		m.wait.Unknown++
		return
	}
	m.wait.add(end.Sub(start).Seconds(), source)
}

func (m *sessionMetrics) waitMetric(complete bool) Metric {
	r := m.wait
	if !m.waitStart.IsZero() {
		r.Unknown++
	}
	if !complete {
		r.Unknown++
	}
	if !r.Known && r.Unknown == 0 {
		r.add(0, "measured")
	}
	return r
}
