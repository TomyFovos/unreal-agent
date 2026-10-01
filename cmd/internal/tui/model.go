// Package tui is a disposable projection of Host history and snapshots. It never
// opens a Session store and never performs model/tool work.
package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type Reader interface {
	Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error)
	Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error)
}
type Snapshot struct {
	ID                 session.ID
	Generation         string
	Revision           uint64
	After              sessionstore.Sequence
	Running, Connected bool
	Status             string
	Lines              []string
	Operations         []operation.Operation
	Progress           *host.Progress
}
type Model struct {
	mu    sync.Mutex
	state Snapshot
}

func NewModel(id session.ID) *Model { return &Model{state: Snapshot{ID: id, Status: "connecting"}} }
func (m *Model) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.Lines = append([]string(nil), s.Lines...)
	s.Operations = append([]operation.Operation(nil), s.Operations...)
	if s.Progress != nil {
		p := *s.Progress
		s.Progress = &p
	}
	return s
}
func (m *Model) status(s string, connected bool) {
	m.mu.Lock()
	m.state.Status = s
	m.state.Connected = connected
	if !connected {
		m.state.Progress = nil
	}
	m.mu.Unlock()
}
func (m *Model) apply(v host.View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &m.state
	for _, item := range v.History.Items {
		if item.Sequence <= s.After {
			continue
		}
		if item.Sequence != s.After+1 {
			return errors.New("history gap")
		}
		s.After = item.Sequence
		for _, line := range itemLines(item) {
			if len(line) > 4096 {
				line = strings.ToValidUTF8(line[:4096], "") + " [display clipped; canonical history retained]"
			}
			s.Lines = append(s.Lines, line)
		}
		// Display is a bounded window. Canonical history remains available by paging.
		if len(s.Lines) > 1024 {
			s.Lines = append([]string(nil), s.Lines[len(s.Lines)-1024:]...)
		}
	}
	if v.Generation != s.Generation || v.Revision >= s.Revision {
		s.Generation = v.Generation
		s.Revision = v.Revision
		s.Running = v.Running
		s.Operations = v.Operations
		s.Progress = v.Progress
	}
	s.Connected = true
	s.Status = "connected"
	if !s.Running {
		s.Status = "stopped; /resume continues this session"
	}
	if v.Failure != "" {
		s.Status = "session failed: " + v.Failure
	}
	return nil
}
func (m *Model) progress(e host.Event) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &m.state
	if e.Generation != s.Generation || e.Revision != s.Revision+1 {
		return false
	}
	s.Revision = e.Revision
	s.Progress = e.Progress
	return true
}
func itemLines(item host.HistoryItem) []string {
	switch item.Kind {
	case sessionstore.ItemInput:
		input := item.Data.(inbox.Input)
		if input.Kind == inbox.InputExternal {
			var text string
			if json.Unmarshal(input.Payload, &text) == nil {
				return []string{"you> " + text}
			}
		}
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse).Response
		var result []string
		for _, item := range response.Output {
			if item.Type == llm.ItemMessage {
				result = append(result, "agent> "+item.Data.(llm.Message).Text)
			}
		}
		if response.Failure != nil {
			result = append(result, "error> "+response.Failure.Message)
		}
		return result
	}
	return nil
}

// Watch batches canonical history refreshes independently of rendering. Events
// are hints; history cursors and latest operation snapshots are authoritative.
func Watch(ctx context.Context, r Reader, m *Model, changed func()) error {
	for ctx.Err() == nil {
		s := m.Snapshot()
		sub, err := r.Subscribe(ctx, s.ID, s.After, 128, 32)
		if err != nil {
			m.status("disconnected; reconnecting", false)
			changed()
			if !pause(ctx) {
				break
			}
			continue
		}
		err = m.apply(sub.Initial)
		for more := sub.Initial.History.More; err == nil && more; {
			s = m.Snapshot()
			v, e := r.Inspect(ctx, s.ID, s.After, 128)
			err = e
			if err == nil {
				err = m.apply(v)
				more = v.History.More
			}
		}
		changed()
		if err != nil {
			sub.Cancel()
			m.status("resync required", false)
			changed()
			if !pause(ctx) {
				break
			}
			continue
		}
		reconnect := false
		for !reconnect {
			select {
			case <-ctx.Done():
				sub.Cancel()
				return ctx.Err()
			case e, ok := <-sub.Events:
				if !ok || e.Kind == "gap" || e.Kind == "disconnected" {
					if ok || m.Snapshot().Running {
						m.status("gap/disconnect; resyncing", false)
					}
					changed()
					reconnect = true
					continue
				}
				s = m.Snapshot()
				if e.Generation == s.Generation && e.Revision <= s.Revision {
					continue
				}
				if e.Kind == "progress" && m.progress(e) {
					changed()
					continue
				}
				for {
					s = m.Snapshot()
					v, e := r.Inspect(ctx, s.ID, s.After, 128)
					if e != nil {
						err = e
						break
					}
					if err = m.apply(v); err != nil || !v.History.More {
						break
					}
				}
				changed()
				if err != nil {
					reconnect = true
				}
			}
		}
		sub.Cancel()
		if !pause(ctx) {
			break
		}
	}
	return ctx.Err()
}
func pause(ctx context.Context) bool {
	t := time.NewTimer(250 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// SafeText strips terminal control sequences' introducers/control bytes. All
// provider/tool/user text is data, never a terminal command.
func SafeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == 0x202e || r == 0x202d {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}
func Wrap(s string, width int) []string {
	width = max(1, width)
	var result []string
	for _, line := range strings.Split(SafeText(s), "\n") {
		var b strings.Builder
		cells := 0
		g := uniseg.NewGraphemes(strings.ReplaceAll(line, "\t", "    "))
		for g.Next() {
			text := g.Str()
			w := g.Width()
			if cells+w > width && cells > 0 {
				result = append(result, b.String())
				b.Reset()
				cells = 0
			}
			if w > width {
				text = "�"
				w = 1
			}
			b.WriteString(text)
			cells += w
		}
		result = append(result, b.String())
	}
	return result
}
func Render(s Snapshot, input, status string, width, height int, panel string) string {
	width = max(1, min(width, 512))
	height = max(4, min(height, 200))
	var lines []string
	lines = append(lines, Wrap(fmt.Sprintf("Unreal Agent | %s | %s", s.ID, s.Status), width)[0])
	var body []string
	for _, line := range s.Lines {
		body = append(body, Wrap(line, width)...)
	}
	if s.Progress != nil {
		label := "waiting for completed response"
		if s.Progress.Mode == "streaming" {
			label = "streaming (temporary)"
		}
		body = append(body, Wrap("["+label+"] "+s.Progress.Text, width)...)
	}
	ops := append([]operation.Operation(nil), s.Operations...)
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	for _, op := range ops {
		body = append(body, Wrap(fmt.Sprintf("tool> %s %s %s", op.ID, op.ToolName, op.Status), width)...)
	}
	if panel != "" {
		body = append(body, Wrap(panel, width)...)
	}
	statusLines := Wrap(status, width)
	if bound := min(6, height-3); len(statusLines) > bound {
		statusLines = statusLines[:bound]
	}
	footer := len(statusLines) + 2
	room := max(0, height-len(lines)-footer)
	if len(body) > room {
		body = body[len(body)-room:]
	}
	lines = append(lines, body...)
	for len(lines) < height-footer {
		lines = append(lines, "")
	}
	lines = append(lines, statusLines...)
	lines = append(lines, Wrap("Ctrl-D detach | Ctrl-C stop | /resume /retry /login /logout /methods", width)[0])
	prompt := Wrap(input, width)
	lines = append(lines, prompt[len(prompt)-1])
	return "\x1b[H\x1b[2J" + strings.Join(lines, "\r\n")
}
