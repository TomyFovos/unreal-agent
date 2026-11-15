package claudeagent

import (
	"encoding/json/v2"
	"strconv"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const streamActivityInterval = 60 * time.Second

// streamObservation is encoded in ActivityRecord.Reason to keep this adapter's
// diagnostics within the existing activity schema. Estimates describe visible
// UTF-8 bytes / 4, rounded up, and never enter usage or generation statistics.
type streamObservation struct {
	Event                  string           `json:"event"`
	ParentToolUseID        string           `json:"parent_tool_use_id,omitzero"`
	InputTokens            *int64           `json:"input_tokens,omitzero"`
	DeltaCount             *int64           `json:"delta_count,omitzero"`
	ThinkingTokensEstimate *int64           `json:"thinking_tokens_estimate,omitzero"`
	TextTokensEstimate     *int64           `json:"text_tokens_estimate,omitzero"`
	LastDeltaAt            time.Time        `json:"last_delta_at,omitzero"`
	OutputTokens           *int64           `json:"output_tokens,omitzero"`
	ReasoningTokens        *int64           `json:"reasoning_tokens,omitzero"`
	StopReason             string           `json:"stop_reason,omitzero"`
	OutputPhase            string           `json:"output_phase,omitzero"`
	Count                  int64            `json:"count,omitzero"`
	Attempt                int              `json:"attempt,omitzero"`
	MaxRetries             int              `json:"max_retries,omitzero"`
	RetryDelayMS           int64            `json:"retry_delay_ms,omitzero"`
	ErrorStatus            *int             `json:"error_status,omitzero"`
	ErrorType              string           `json:"error_type,omitzero"`
	NoResponse             *noResponseRetry `json:"no_response,omitzero"`
}

type streamRequestActivity struct {
	id, model                string
	started, lastDelta       time.Time
	deltas, reported         int64
	thinkingBytes, textBytes int64
	output, reasoning        *int64
	input                    *usage
	stopReason               string
	phase                    string
}

// Native Agent streams have independent parent_tool_use_id values. Keep them
// separate so a busy native child cannot make a silent main stream look active.
type streamActivities struct {
	mu        sync.Mutex
	turnID    string
	mainModel string
	seq       int
	active    map[string]*streamRequestActivity
	counts    map[string]map[string]int64
	limits    map[string][]outputLimitHit
	usage     map[string]llm.Usage
	now       func() time.Time
	record    func(llm.ActivityRecord)
}

func (t *turn) observeStreams(interval time.Duration) (*streamActivities, func()) {
	s := &streamActivities{turnID: t.opt.Identity.TurnID, active: map[string]*streamRequestActivity{}, counts: map[string]map[string]int64{}, limits: map[string][]outputLimitHit{}, now: t.now, record: t.recordActivity}
	if t.opt.RecordActivity == nil {
		return s, func() {}
	}
	ticker := time.NewTicker(interval)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.progress()
			}
		}
	}()
	return s, func() {
		ticker.Stop()
		close(stop)
		<-done
	}
}

func (s *streamActivities) observe(f frame) {
	if s == nil {
		return
	}
	scope := ""
	if f.ParentToolUseID != nil {
		scope = *f.ParentToolUseID
	}
	var event streamEvent
	if f.Type == "stream_event" && json.Unmarshal(f.Event, &event) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	r := s.active[scope]
	if f.Type == "system" && (f.Subtype == "api_retry" || f.Subtype == "api_error") {
		observation := streamObservation{Event: f.Subtype, Attempt: f.Attempt, MaxRetries: f.MaxRetries, RetryDelayMS: f.RetryDelayMS, ErrorStatus: f.ErrorStatus, NoResponse: f.NoResponse}
		if f.Subtype == "api_error" {
			var detail struct {
				Status *int `json:"status"`
			}
			if json.Unmarshal(f.Error, &detail) == nil {
				observation.ErrorStatus = detail.Status
			}
		}
		s.notice(scope, r, observation, now)
		return
	}
	if f.Type != "stream_event" {
		return
	}
	switch event.Type {
	case "message_start":
		s.seq++
		r = &streamRequestActivity{id: s.turnID + "/stream/" + strconv.Itoa(s.seq), model: event.Message.Model, started: now}
		if scope == "" && r.model != "" && r.model != "<synthetic>" {
			s.mainModel = r.model
		}
		s.active[scope] = r
		s.counts[scope] = map[string]int64{}
		observation := streamObservation{Event: "message_start"}
		if event.Message.Usage != nil {
			input := event.Message.Usage.inputTokens()
			observation.InputTokens = &input
			r.input = event.Message.Usage
		}
		s.emit(scope, r, observation, time.Time{})
	case "content_block_start":
		if r != nil {
			r.phase = ""
			if event.ContentBlock.Type == "thinking" || event.ContentBlock.Type == "text" {
				r.phase = event.ContentBlock.Type
			}
		}
	case "content_block_delta", "message_delta":
		if r == nil {
			return
		}
		r.deltas++
		r.lastDelta = now
		if event.Type == "content_block_delta" {
			switch event.Delta.Type {
			case "thinking_delta":
				r.thinkingBytes += int64(len(event.Delta.Thinking))
				r.phase = "thinking"
			case "text_delta":
				r.textBytes += int64(len(event.Delta.Text))
				r.phase = "text"
			case "input_json_delta":
				r.phase = ""
			}
		} else {
			if event.Usage != nil {
				output := event.Usage.OutputTokens
				r.output = &output
				if reasoning, known := event.Usage.reasoningTokens(); known {
					r.reasoning = &reasoning
				}
			}
			if event.Delta.StopReason != "" {
				r.stopReason = event.Delta.StopReason
			}
		}
	case "message_stop":
		if r != nil {
			if r.input != nil && r.output != nil && r.model != "<synthetic>" {
				if s.usage == nil {
					s.usage = map[string]llm.Usage{}
				}
				u := *r.input
				u.OutputTokens, u.ReasoningTokens = *r.output, r.reasoning
				s.usage[r.model] = addUsage(s.usage[r.model], reportedUsage(u))
			}
			observation := r.snapshot("message_stop")
			observation.StopReason = r.stopReason
			if r.stopReason == "max_tokens" || r.stopReason == "max_output_tokens" {
				if s.limits == nil {
					s.limits = map[string][]outputLimitHit{}
				}
				hit := outputLimitHit{phase: r.phase}
				if r.output != nil {
					hit.tokens = *r.output
				}
				s.limits[scope] = append(s.limits[scope], hit)
			}
			s.emit(scope, r, observation, now)
			delete(s.active, scope)
		}
	case "error":
		s.notice(scope, r, streamObservation{Event: "error", ErrorType: event.Error.Type}, now)
	}
}

func (s *streamActivities) notice(scope string, r *streamRequestActivity, observation streamObservation, now time.Time) {
	if s.counts[scope] == nil {
		s.counts[scope] = map[string]int64{}
	}
	kind := observation.Event + "/" + observation.ErrorType
	s.counts[scope][kind]++
	observation.Count = s.counts[scope][kind]
	s.emit(scope, r, observation, now)
}

func (s *streamActivities) progress() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for scope, r := range s.active {
		observation := streamObservation{Event: "no_delta"}
		if r.deltas != r.reported {
			observation = r.snapshot("progress")
			r.reported = r.deltas
		}
		s.emit(scope, r, observation, now)
	}
}

func (r *streamRequestActivity) snapshot(event string) streamObservation {
	deltas := r.deltas
	observation := streamObservation{Event: event, DeltaCount: &deltas, LastDeltaAt: r.lastDelta, OutputTokens: r.output, ReasoningTokens: r.reasoning, OutputPhase: r.phase}
	if r.thinkingBytes > 0 {
		estimate := (r.thinkingBytes + 3) / 4
		observation.ThinkingTokensEstimate = &estimate
	}
	if r.textBytes > 0 {
		estimate := (r.textBytes + 3) / 4
		observation.TextTokensEstimate = &estimate
	}
	return observation
}

func (s *streamActivities) emit(scope string, r *streamRequestActivity, observation streamObservation, at time.Time) {
	observation.ParentToolUseID = scope
	data, _ := json.Marshal(observation)
	record := llm.ActivityRecord{Kind: "claude_stream", ID: s.turnID, Provider: ProviderID, EndedAt: at, Reason: string(data)}
	if r != nil {
		record.ID, record.Model, record.StartedAt = r.id, r.model, r.started
	}
	s.record(record)
}
