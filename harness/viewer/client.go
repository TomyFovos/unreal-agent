package viewer

import (
	"context"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"sync"
	"time"
)

var ErrWatching = errors.New("a viewer is already watching this session")

type Notice struct {
	SessionID session.ID
	Err       error
}

// Client owns one observation stream per session; callbacks run in Watch's
// goroutine and must return promptly. Model may be read concurrently by a UI.
type Client struct {
	Reader     Reader
	Model      *Model
	Controls   Controls
	PageSize   int
	Capacity   int
	RetryDelay time.Duration
	mu         sync.Mutex
	watching   map[session.ID]bool
}

func NewClient(reader Reader, model *Model, controls Controls) *Client {
	if model == nil {
		model = New(Options{})
	}
	return &Client{Reader: reader, Model: model, Controls: controls, PageSize: 128, Capacity: 128, RetryDelay: 250 * time.Millisecond,
		watching: map[session.ID]bool{}}
}
func (c *Client) sizes() (int, int) {
	page, capacity := c.PageSize, c.Capacity
	if page < 1 || page > 4096 {
		page = 128
	}
	if capacity < 1 || capacity > 4096 {
		capacity = 128
	}
	return page, capacity
}
func (c *Client) enter(id session.ID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watching[id] {
		return ErrWatching
	}
	c.watching[id] = true
	return nil
}
func (c *Client) leave(id session.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.watching, id)
	c.Model.mu.Lock()
	defer c.Model.mu.Unlock()
	if p := c.Model.sessions[id]; p != nil && p.runtime != RuntimeUnknown {
		p.runtime = RuntimeUnknown
		c.Model.version++
	}
}
func (c *Client) pages(ctx context.Context, id session.ID, v host.View) error {
	if err := c.Model.Replace(id, v); err != nil {
		return err
	}
	page, _ := c.sizes()
	for v.History.More {
		after := v.History.NextAfter
		next, err := c.Reader.Inspect(ctx, id, after, page)
		if err != nil {
			return err
		}
		if err = c.Model.AppendPage(id, after, next); err != nil {
			return err
		}
		v = next
	}
	return nil
}

func (c *Client) observe(ctx context.Context, id session.ID) (host.View, bool, error) {
	reader, ok := c.Reader.(ObservationReader)
	if !ok {
		return host.View{}, false, nil
	}
	v, err := reader.Observe(ctx, id, c.Model.options.RecentLimit)
	if errors.Is(err, ErrUnavailable) {
		return host.View{}, false, nil
	}
	return v, true, err
}

// Refresh reads canonical state without starting or resuming any runtime.
func (c *Client) Refresh(ctx context.Context, id session.ID) error {
	if c.Reader == nil {
		return ErrUnavailable
	}
	if err := c.enter(id); err != nil {
		return err
	}
	defer func() { c.mu.Lock(); delete(c.watching, id); c.mu.Unlock() }()
	page, _ := c.sizes()
	c.Model.mu.Lock()
	previous := c.Model.sessions[id]
	after := sessionstore.BeforeFirst
	generation := ""
	if previous != nil && !previous.resync && !previous.more {
		after, generation = previous.cursor, previous.view.Generation
	}
	c.Model.mu.Unlock()
	inspect := func() (host.View, error) {
		if after == 0 {
			if v, used, err := c.observe(ctx, id); used {
				return v, err
			}
		}
		return c.Reader.Inspect(ctx, id, after, page)
	}
	v, err := inspect()
	if after != 0 && (err != nil || v.Generation != generation || !validPage(after, v.History)) {
		after = 0
		v, err = inspect()
	}
	if err != nil {
		c.Model.Disconnect(id)
		return err
	}
	if after == 0 {
		err = c.pages(ctx, id, v)
	} else {
		err = c.Model.AppendPage(id, after, v)
		for err == nil && v.History.More {
			after = v.History.NextAfter
			v, err = c.Reader.Inspect(ctx, id, after, page)
			if err == nil {
				err = c.Model.AppendPage(id, after, v)
			}
		}
	}
	if err != nil {
		c.Model.Invalidate(id)
	}
	return err
}

// History returns an explicit bounded persisted transcript page without changing
// the live projection or token aggregates.
func (c *Client) History(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.HistoryPage, error) {
	if c.Reader == nil {
		return host.HistoryPage{}, ErrUnavailable
	}
	if limit < 1 || limit > 4096 {
		return host.HistoryPage{}, ErrInvalidPage
	}
	v, err := c.Reader.Inspect(ctx, id, after, limit)
	if err != nil {
		return host.HistoryPage{}, err
	}
	if v.Session.Session.ID != id || !validPage(after, v.History) {
		return host.HistoryPage{}, ErrInvalidPage
	}
	return copyValue(v.History)
}

// Watch resubscribes from canonical state after a gap or disconnected live
// stream. A stopped/read-only snapshot completes after its history is loaded.
func (c *Client) Watch(ctx context.Context, id session.ID, changed func(Notice)) error {
	if c.Reader == nil {
		return ErrUnavailable
	}
	if err := c.enter(id); err != nil {
		return err
	}
	defer c.leave(id)
	notify := func(err error) {
		if changed != nil {
			changed(Notice{SessionID: id, Err: err})
		}
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		page, capacity := c.sizes()
		c.Model.mu.Lock()
		after := sessionstore.BeforeFirst
		generation := ""
		stopped := false
		var revision uint64
		if p := c.Model.sessions[id]; p != nil && !p.resync && !p.more {
			after, generation = p.cursor, p.view.Generation
			stopped, revision = !p.view.Running, p.view.Revision
		}
		c.Model.mu.Unlock()
		if stopped && generation != "" {
			if reader, ok := c.Reader.(VersionReader); ok {
				if version, err := reader.Version(ctx, id); err == nil && version.Generation == generation && version.Revision == revision {
					return nil
				}
			}
		}
		version := c.Model.Version()
		subscribe := func() (host.Subscription, error) {
			if after == 0 {
				v, used, err := c.observe(ctx, id)
				if used {
					if err != nil {
						return host.Subscription{}, err
					}
					if err = c.Model.Replace(id, v); err != nil {
						return host.Subscription{}, err
					}
					after, generation = v.History.NextAfter, v.Generation
				}
			}
			return c.Reader.Subscribe(ctx, id, after, page, capacity)
		}
		sub, err := subscribe()
		if after != 0 && (err != nil || sub.Initial.Generation != generation || !validPage(after, sub.Initial.History)) {
			if err == nil && sub.Cancel != nil {
				sub.Cancel()
			}
			after = 0
			sub, err = subscribe()
		}
		if err == nil {
			if sub.Cancel == nil {
				sub.Cancel = func() {}
			}
			if after == 0 {
				err = c.pages(ctx, id, sub.Initial)
			} else {
				v := sub.Initial
				err = c.Model.AppendPage(id, after, v)
				for err == nil && v.History.More {
					after = v.History.NextAfter
					v, err = c.Reader.Inspect(ctx, id, after, page)
					if err == nil {
						err = c.Model.AppendPage(id, after, v)
					}
				}
			}
			if err == nil {
				if c.Model.Version() != version {
					notify(nil)
				}
				if !sub.Initial.Running {
					sub.Cancel()
					return nil
				}
				err = c.consume(ctx, id, sub, notify)
			}
			sub.Cancel()
			if err == nil {
				return nil
			}
		}
		if context.Cause(ctx) != nil {
			return context.Cause(ctx)
		}
		c.Model.Invalidate(id)
		notify(err)
		delay := c.RetryDelay
		if delay <= 0 {
			delay = 250 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}
func (c *Client) consume(ctx context.Context, id session.ID, sub host.Subscription, notify func(error)) error {
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case event, ok := <-sub.Events:
			if !ok {
				return ErrResync
			}
			err := c.Model.Apply(id, event)
			if errors.Is(err, ErrStale) {
				continue
			}
			if err != nil {
				return err
			}
			notify(nil)
			if event.Kind == "stopped" {
				return nil
			}
		}
	}
}
func (c *Client) request(child session.ID, inputID string, text string) (ControlRequest, error) {
	if inputID == "" {
		return ControlRequest{}, fmt.Errorf("control requires a stable input ID")
	}
	detail, ok := c.Model.Detail(child, time.Now())
	if !ok || detail.Row.ParentID == "" || detail.Row.Problem != "" || detail.Row.Finish != nil {
		return ControlRequest{}, ErrUnavailable
	}
	parent, ok := c.Model.Detail(detail.Row.ParentID, time.Now())
	if !ok || parent.Row.Generation == "" || parent.Row.Runtime != RuntimeRunning || parent.Row.NeedsResync || parent.Row.Problem != "" || terminal(detail.Row.ParentOperationStatus) {
		return ControlRequest{}, ErrUnavailable
	}
	return ControlRequest{ParentID: parent.Row.ID, ParentGeneration: parent.Row.Generation, OperationID: detail.Row.ParentOperationID, ChildID: child, InputID: inbox.ID(inputID), Text: text}, nil
}

// Steer, Cancel and Resume only call the owning Host control adapter. Success
// does not optimistically change displayed state; wait for committed events.
func (c *Client) Steer(ctx context.Context, child session.ID, inputID, text string) (host.Receipt, error) {
	if text == "" {
		return host.Receipt{}, fmt.Errorf("steering text is empty")
	}
	return c.control(ctx, "steer", child, inputID, text)
}
func (c *Client) Cancel(ctx context.Context, child session.ID, inputID, reason string) (host.Receipt, error) {
	return c.control(ctx, "cancel", child, inputID, reason)
}
func (c *Client) Resume(ctx context.Context, child session.ID, inputID string) error {
	_, err := c.control(ctx, "resume", child, inputID, "")
	return err
}
func (c *Client) control(ctx context.Context, action string, child session.ID, inputID, text string) (host.Receipt, error) {
	if c.Controls == nil {
		return host.Receipt{}, ErrUnavailable
	}
	req, err := c.request(child, inputID, text)
	if err != nil {
		return host.Receipt{}, err
	}
	var receipt host.Receipt
	switch action {
	case "steer":
		receipt, err = c.Controls.SteerChild(ctx, req)
	case "cancel":
		receipt, err = c.Controls.CancelChild(ctx, req)
	case "resume":
		err = c.Controls.ResumeChild(ctx, req)
	}
	return receipt, err
}
