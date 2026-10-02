package host

import (
	"context"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Progress is an ephemeral bounded snapshot. Generation is carried by View/Event;
// Epoch disambiguates multiple model calls and retries within the same turn.
type Progress struct {
	TurnID          session.TurnID
	Epoch, Attempt  uint64
	Mode, Text      string
	Done, Truncated bool
}

const maxProgressBytes = 64 << 10

type progressAdapter struct {
	session *Session
	next    llm.Adapter
}

func (s *Session) withProgress(next llm.Adapter) llm.Adapter { return &progressAdapter{s, next} }
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
	s.progress = &Progress{TurnID: turn, Epoch: epoch, Mode: "completed_response"}
	s.broadcast(Event{Kind: "progress", Progress: s.progress})
	s.mu.Unlock()
	lastBroadcast := time.Time{}
	done := false // accessed only with s.mu
	prior := o.Progress
	o.Progress = func(p llm.Progress) {
		s.mu.Lock()
		if !done && ctx.Err() == nil && s.running && s.currentTurn() == turn && s.progress != nil && s.progress.Epoch == epoch {
			current := s.progress
			if p.Attempt >= current.Attempt {
				if p.Reset || p.Attempt > current.Attempt {
					current.Text = ""
					current.Truncated = false
				}
				current.Mode = "streaming"
				current.Attempt = p.Attempt
				remaining := maxProgressBytes - len(current.Text)
				delta := p.Delta
				if len(delta) > remaining {
					delta = delta[:remaining]
					current.Truncated = true
				}
				current.Text = strings.ToValidUTF8(current.Text+delta, "")
				if p.Reset || lastBroadcast.IsZero() || time.Since(lastBroadcast) >= 30*time.Millisecond {
					s.broadcast(Event{Kind: "progress", Progress: current})
					lastBroadcast = time.Now()
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
	if s.progress != nil && s.progress.Epoch == epoch {
		s.progress.Done = true
		s.broadcast(Event{Kind: "progress", Progress: s.progress})
	}
	s.mu.Unlock()
	return response, err
}
