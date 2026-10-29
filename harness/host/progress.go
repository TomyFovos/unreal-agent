package host

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Progress is an ephemeral snapshot. Generation is carried by View/Event;
// Epoch disambiguates multiple model calls and retries within the same turn.
type Progress struct {
	TurnID          session.TurnID
	Epoch, Attempt  uint64
	Mode, Text      string
	Done, Truncated bool
	Statistics      *llm.GenerationStatistics `json:",omitzero"`
}

type progressAdapter struct {
	session *Session
	next    llm.Adapter
	now     func() time.Time
}

func (a *progressAdapter) RequiresFullToolBatch() bool { return llm.RequiresToolBatch(a.next) }

// ModelBusy reports whether a model call of this Session is in flight, and
// a channel that closes when that changes.
func (s *Session) ModelBusy() (bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.respondingChanged == nil {
		s.respondingChanged = make(chan struct{})
	}
	return s.responding > 0, s.respondingChanged
}

// respondingLocked records a model call starting or ending.
func (s *Session) respondingLocked(delta int) {
	s.responding += delta
	if s.respondingChanged != nil {
		close(s.respondingChanged)
	}
	s.respondingChanged = make(chan struct{})
}

func (s *Session) withProgress(next llm.Adapter) llm.Adapter {
	return &progressAdapter{session: s, next: next}
}
func (s *Session) currentTurn() session.TurnID {
	for i := len(s.items) - 1; i >= 0; i-- {
		if s.items[i].Kind == sessionstore.ItemTurn {
			return s.items[i].Data.(session.Turn).ID
		}
	}
	return ""
}
func (a *progressAdapter) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	s := a.session
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return llm.Response{}, err
	}
	if !s.running {
		s.mu.Unlock()
		return llm.Response{}, ErrStopped
	}
	s.progressEpoch++
	epoch := s.progressEpoch
	turn := s.currentTurn()
	origin := turn
	s.respondingLocked(1)
	defer func() { s.mu.Lock(); s.respondingLocked(-1); s.mu.Unlock() }()
	s.progress = &Progress{TurnID: turn, Epoch: epoch, Mode: "completed_response"}
	o.ConversationTokens = s.conversationTokensLocked()
	s.broadcast(Event{Kind: "progress", Progress: s.progress})
	s.mu.Unlock()
	o.RecordConversation = s.recordConversation
	lastBroadcast := time.Time{}
	now := a.now
	if now == nil {
		now = time.Now
	}
	started := now()
	statistics := func(response llm.Response) *llm.GenerationStatistics {
		if response.Generation != nil && response.Generation.Valid() {
			copy := *response.Generation
			copy.Model = r.Model.ID
			return &copy
		}
		stats := &llm.GenerationStatistics{Model: r.Model.ID, Tokens: llm.ReportedTokens(response.Usage), DurationMicros: now().Sub(started).Microseconds(), Source: "client_clock"}
		if !stats.Valid() {
			return nil // zero/unmeasurable duration is unknown, not invented timing
		}
		return stats
	}
	pendingUTF8 := ""        // an incomplete rune belongs to the next delta, not to invalid text
	done := false            // accessed only with s.mu
	waitingForTools := false // freeze statistics throughout approval/execution waits
	// Tools and steering can both commit a continuation within one invocation.
	// Its progress follows that new canonical turn. Caller holds s.mu.
	resumeProgress := func() {
		if !done && ctx.Err() == nil && s.running && s.progressEpoch == epoch {
			for i := len(s.items) - 1; i >= 0; i-- {
				if next, ok := s.items[i].Data.(session.Turn); ok {
					if next.ToolContinuation == origin && next.ID != turn {
						s.progressEpoch++
						epoch, turn = s.progressEpoch, next.ID
						pendingUTF8 = ""
						s.progress = &Progress{TurnID: turn, Epoch: epoch, Mode: "completed_response"}
						s.broadcast(Event{Kind: "progress", Progress: s.progress})
					}
					break
				}
			}
		}
	}
	prior := o.Progress
	if steering := o.Steering; steering != nil {
		o.Steering = func(callCtx context.Context) ([]string, error) {
			inputs, err := steering(callCtx)
			if err == nil && len(inputs) > 0 {
				s.mu.Lock()
				resumeProgress()
				s.mu.Unlock()
			}
			return inputs, err
		}
	}
	if tools := o.Tools; tools != nil {
		o.Tools = func(callCtx context.Context, public llm.Response) ([]llm.ToolOutcome, error) {
			s.mu.Lock()
			waitingForTools = true
			public.Generation = statistics(public)
			if !done && s.progress != nil && s.progress.Epoch == epoch {
				s.progress.Statistics = public.Generation
				s.progress.Done = true
				s.broadcast(Event{Kind: "progress", Progress: s.progress})
			}
			s.mu.Unlock()
			outcomes, err := tools(callCtx, public)
			s.mu.Lock()
			waitingForTools = false
			started = now() // approval/Operation/receipt wait is never TPS time
			s.mu.Unlock()
			if err != nil {
				return outcomes, err
			}
			s.mu.Lock()
			resumeProgress()
			s.mu.Unlock()
			return outcomes, nil
		}
	}
	o.Progress = func(p llm.Progress) {
		s.mu.Lock()
		if !done && !waitingForTools && ctx.Err() == nil && s.running && s.currentTurn() == turn && s.progress != nil && s.progress.Epoch == epoch {
			current := s.progress
			if p.Attempt >= current.Attempt {
				if p.Reset || p.Attempt > current.Attempt {
					current.Text = ""
					pendingUTF8 = ""
					current.Truncated = false
					current.Statistics = nil
					started = now()
				}
				phaseChanged := current.Mode != p.Phase && (current.Mode == "compacting" || p.Phase != "")
				current.Mode = "streaming"
				if p.Phase != "" {
					current.Mode = p.Phase
				}
				current.Attempt = p.Attempt
				if p.Tokens != nil {
					tokens := p.Tokens.Safe()
					if current.Statistics != nil {
						old := current.Statistics.Tokens
						if !tokens.InputKnown {
							tokens.Input, tokens.InputKnown = old.Input, old.InputKnown
						}
						if !tokens.OutputKnown {
							tokens.Output, tokens.OutputKnown = old.Output, old.OutputKnown
						}
					}
					current.Statistics = &llm.GenerationStatistics{Model: r.Model.ID, Tokens: tokens, DurationMicros: now().Sub(started).Microseconds(), Source: "client_clock"}
				}
				pendingUTF8 += p.Delta
				end := 0
				for end < len(pendingUTF8) && utf8.FullRuneInString(pendingUTF8[end:]) {
					_, size := utf8.DecodeRuneInString(pendingUTF8[end:])
					end += size
				}
				current.Text += strings.ToValidUTF8(pendingUTF8[:end], "")
				pendingUTF8 = pendingUTF8[end:]
				if p.Reset || phaseChanged || lastBroadcast.IsZero() || now().Sub(lastBroadcast) >= 30*time.Millisecond {
					s.broadcast(Event{Kind: "progress", Progress: current})
					lastBroadcast = now()
				}
			}
		}
		s.mu.Unlock()
		if prior != nil {
			prior(p)
		}
	}
	response, err := a.next.Respond(ctx, r, o)
	s.mu.Lock()
	done = true
	if err == nil && response.Failure == nil {
		response.Generation = statistics(response)
	}
	if s.progress != nil && s.progress.Epoch == epoch {
		s.progress.Done = true
		if response.Generation != nil {
			s.progress.Statistics = response.Generation
		}
		s.broadcast(Event{Kind: "progress", Progress: s.progress})
	}
	s.mu.Unlock()
	return response, err
}
