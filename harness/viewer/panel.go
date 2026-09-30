package viewer

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/rivo/uniseg"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type panelRequest struct {
	action   string
	child    session.ID
	id, text string
}

// Panel supplies render/command/update hooks without owning the frontend's
// terminal or editor. Close joins all read subscriptions; it never stops a Host.
type Panel struct {
	client       *Client
	parent       session.ID
	ctx          context.Context
	cancel       context.CancelFunc
	workers      sync.WaitGroup
	updates      chan struct{}
	mu           sync.Mutex
	transcript   *sessionstore.Page
	transcriptID session.ID
	retry        *panelRequest
	problem      string
}

// NewPanel watches the parent and refreshes only the selected child's canonical
// snapshot once per second. It does not send polling input to either agent.
func NewPanel(ctx context.Context, parent session.ID, client *Client) *Panel {
	ctx, cancel := context.WithCancel(ctx)
	p := &Panel{client: client, parent: parent, ctx: ctx, cancel: cancel, updates: make(chan struct{}, 1)}
	p.workers.Go(func() {
		for ctx.Err() == nil {
			_ = client.Watch(ctx, parent, func(n Notice) { p.setProblem(n.Err); p.notify() })
			if !pause(ctx, time.Second) {
				return
			}
		}
	})
	p.workers.Go(func() {
		for pause(ctx, time.Second) {
			id := client.Model.Selected()
			if id != "" && id != parent {
				err := client.Refresh(ctx, id)
				if !errors.Is(err, ErrWatching) {
					p.setProblem(err)
					p.notify()
				}
			}
		}
	})
	return p
}
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (p *Panel) Close()                   { p.cancel(); p.workers.Wait() }
func (p *Panel) Updates() <-chan struct{} { return p.updates }
func (p *Panel) notify() {
	select {
	case p.updates <- struct{}{}:
	default:
	}
}
func (p *Panel) setProblem(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.problem = ""
	if err != nil {
		p.problem = SafeText(err.Error())
	}
}
func (p *Panel) Render(width, height int) string {
	rows := p.client.Model.Rows(time.Now())
	selected := p.client.Model.Selected()
	if len(rows) == 0 {
		return clipLine("Children: loading canonical parent state", max(1, width))
	}
	var b strings.Builder
	b.WriteString("Children: /child ID /child-history /child-send TEXT /child-cancel /child-resume\n")
	count := max(1, min(6, height/4))
	start := 0
	for i, r := range rows {
		if r.ID == selected && i >= count {
			start = i - count + 1
			break
		}
	}
	end := min(len(rows), start+count)
	b.WriteString(RenderRows(rows[start:end], selected))
	if d, ok := p.client.Model.Detail(selected, time.Now()); ok && selected != p.parent {
		if m := d.Row.ProjectInstructions; m != nil {
			fmt.Fprintf(&b, "Project instructions: %s %s bytes=%d digest=%s\n", SafeText(string(m.SourceKind)), SafeText(m.SourcePath), m.ByteLength, SafeText(m.Digest))
		}
		if d.Row.Finish != nil {
			fmt.Fprintf(&b, "Finish: %s: %s\n", d.Row.Finish.Status, SafeText(d.Row.Finish.Summary))
		}
		if d.Row.Activity != "" {
			fmt.Fprintf(&b, "Activity: %s\n", SafeText(d.Row.Activity))
		}
		for _, op := range d.Row.Operations {
			fmt.Fprintf(&b, "  operation %s [%s] %s\n", SafeText(string(op.ID)), SafeText(op.Tool), op.Status)
			if b.Len() > 8192 {
				break
			}
		}
	}
	p.mu.Lock()
	if p.transcript != nil && p.transcriptID == selected {
		fmt.Fprintf(&b, "History next=%d more=%t (/child-next)\n", p.transcript.NextAfter, p.transcript.More)
		for _, item := range p.transcript.Items {
			fmt.Fprintf(&b, "%d %s\n", item.Sequence, describe(item))
		}
	}
	if p.problem != "" {
		fmt.Fprintf(&b, "Observation: %s\n", p.problem)
	}
	p.mu.Unlock()
	// Keep the panel bounded even when an external summary or line is huge.
	lines := strings.Split(b.String(), "\n")
	limit := max(1, min(24, height/2))
	if len(lines) > limit {
		lines = lines[:limit]
	}
	for i, line := range lines {
		lines[i] = clipLine(line, max(1, width))
	}
	return strings.Join(lines, "\n")
}

// clipLine measures terminal columns and never splits a grapheme cluster.
func clipLine(line string, width int) string {
	graphemes := uniseg.NewGraphemes(line)
	columns, end := 0, 0
	for graphemes.Next() {
		columns += graphemes.Width()
		if columns > width {
			break
		}
		_, end = graphemes.Positions()
	}
	return line[:end]
}
func describe(item sessionstore.Item) string {
	text := string(item.Kind)
	switch d := item.Data.(type) {
	case inbox.Input:
		if d.Kind == inbox.InputExternal {
			var value string
			if json.Unmarshal(d.Payload, &value) == nil {
				text = "human: " + value
			}
		} else if d.Kind == inbox.InputPeer {
			if peer, err := d.DecodePeerMessage(); err == nil {
				text = "peer " + peer.Sender + " [" + peer.Kind + "]: " + peer.Text
			}
		} else {
			text = "input: " + string(d.Kind)
		}
	case sessionstore.ModelResponse:
		var parts []string
		for _, out := range d.Response.Output {
			if msg, ok := out.Data.(llm.Message); ok {
				parts = append(parts, string(msg.Role)+": "+msg.Text)
			}
			if call, ok := out.Data.(llm.ToolCall); ok {
				parts = append(parts, "tool: "+call.Name+" "+call.Arguments)
			}
		}
		text = strings.Join(parts, " | ")
	case sessionstore.ToolCallStatus:
		text = "tool status: " + d.CallID
	case sessionstore.HostRecord:
		text = "host: " + d.Kind
		if d.Finish != nil {
			text = "Finish: " + d.Finish.Result.Status + ": " + d.Finish.Result.Summary
		}
	}
	return SafeText(short(text))
}
func (p *Panel) Command(ctx context.Context, line string) (string, bool) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return "", false
	}
	switch parts[0] {
	case "/children":
		p.notify()
		return "Select a child with /child ID; reads do not resume agents.", true
	case "/child":
		if len(parts) != 2 {
			return "usage: /child ID", true
		}
		id := session.ID(parts[1])
		if err := p.client.Model.Select(id); err != nil {
			return err.Error(), true
		}
		p.mu.Lock()
		p.transcript = nil
		p.mu.Unlock()
		if id != p.parent {
			err := p.client.Refresh(ctx, id)
			if !errors.Is(err, ErrWatching) {
				p.setProblem(err)
			}
		}
		p.notify()
		return "Selected " + SafeText(string(id)), true
	case "/child-history", "/child-next":
		id := p.client.Model.Selected()
		after := sessionstore.Sequence(0)
		limit := 12
		if parts[0] == "/child-next" {
			p.mu.Lock()
			if p.transcript != nil && p.transcriptID == id {
				after = p.transcript.NextAfter
			}
			p.mu.Unlock()
		} else {
			if len(parts) > 3 {
				return "usage: /child-history [after] [limit]", true
			}
			if len(parts) > 1 {
				n, e := strconv.ParseUint(parts[1], 10, 64)
				if e != nil {
					return "invalid history cursor", true
				}
				after = sessionstore.Sequence(n)
			}
			if len(parts) > 2 {
				n, e := strconv.Atoi(parts[2])
				if e != nil || n < 1 || n > 256 {
					return "history limit must be 1..256", true
				}
				limit = n
			}
		}
		page, err := p.client.History(ctx, id, after, limit)
		if err != nil {
			return err.Error(), true
		}
		p.mu.Lock()
		p.transcript = &page
		p.transcriptID = id
		p.mu.Unlock()
		p.notify()
		return fmt.Sprintf("History next=%d more=%t", page.NextAfter, page.More), true
	case "/child-send", "/child-cancel", "/child-resume":
		text := strings.TrimSpace(strings.TrimPrefix(line, parts[0]))
		request := panelRequest{action: parts[0], child: p.client.Model.Selected(), id: uuid.New().String(), text: text}
		return p.execute(ctx, request), true
	case "/child-retry":
		p.mu.Lock()
		request := p.retry
		p.mu.Unlock()
		if request == nil {
			return "No uncertain child request to retry.", true
		}
		return p.execute(ctx, *request), true
	default:
		return "", false
	}
}
func (p *Panel) execute(ctx context.Context, r panelRequest) string {
	var err error
	switch r.action {
	case "/child-send":
		_, err = p.client.Steer(ctx, r.child, r.id, r.text)
	case "/child-cancel":
		_, err = p.client.Cancel(ctx, r.child, r.id, r.text)
	case "/child-resume":
		err = p.client.Resume(ctx, r.child, r.id)
	}
	p.mu.Lock()
	if err != nil {
		p.retry = &r
	} else {
		p.retry = nil
	}
	p.mu.Unlock()
	p.notify()
	if err != nil {
		return "Child request failed: " + SafeText(err.Error()) + "; /child-retry reuses its input ID"
	}
	if r.action == "/child-resume" {
		return "Parent ownership confirmed; existing child recovery is owned by the Host."
	}
	return "Child request committed; canonical state will update when execution advances."
}
