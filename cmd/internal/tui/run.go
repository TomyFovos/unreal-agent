package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type Client interface {
	Reader
	Open(context.Context, host.Mode, session.ID) (host.View, error)
	Submit(context.Context, session.ID, string, inbox.Input) (host.Receipt, error)
	Methods(context.Context) ([]authflow.Support, error)
	Login(context.Context, credential.Reference, credential.Secret) (credential.Metadata, error)
	Logout(context.Context, credential.Reference) error
	ListCredentials(context.Context) ([]credential.Metadata, error)
}
type Config struct {
	Client Client
	ID     session.ID
	Keys   <-chan Key
	Output io.Writer
	Size   func() (int, int)
	Theme  *Theme
	// Legacy Panel remains available to embedders. Live Dock uses structured
	// Viewer data; both hooks are read-only projections, never Session access.
	Panel               func(width, height int) string
	Viewer              func(time.Time) viewer.PanelSnapshot
	ViewChanged         func(ViewMode) // frontend read-only observation scope
	Updates             <-chan struct{}
	Command             func(context.Context, string) (string, bool)
	ModelCatalog        func(context.Context) (modelcatalog.Catalog, error)
	RefreshModelCatalog func(context.Context) (modelcatalog.Catalog, error)
	SelectModel         func(context.Context, string, uint64, sessionstore.RuntimeSelection) error
	ProviderCatalogs    func(context.Context) ([]modelcatalog.Provider, error)
	ProviderCatalog     func(context.Context, string, bool) (modelcatalog.Catalog, error)
	RuntimeCapabilities func(context.Context) (map[string]modelcatalog.Capabilities, error)
	ExportAnalysis      func(context.Context, analysis.Report, string) (string, error)
	// Empty uses the standard private state export directory. No subprocess
	// backend is used for public conversation exports.
	ExportDirectory string
}
type commandResult struct {
	text, kind    string
	err           error
	input         *inbox.Input
	sheet         *Sheet
	picker        *Picker
	transcript    *transcriptPage
	transcriptTop bool
}

func Run(ctx context.Context, c Config) error {
	if c.Client == nil || c.Output == nil || c.Keys == nil {
		return errors.New("tui: client, output and keys required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if c.ViewChanged != nil {
		defer c.ViewChanged(ViewChat)
	}
	m := NewModel(c.ID)
	if c.RuntimeCapabilities != nil {
		if health, err := c.RuntimeCapabilities(ctx); err == nil {
			m.setCapabilities(health)
		}
	}
	initialSync := make(chan struct{})
	var initialSyncOnce sync.Once
	changes := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	notify := func() {
		m.mu.Lock()
		ready := m.state.Connected && !m.historyMore
		m.mu.Unlock()
		if ready {
			initialSyncOnce.Do(func() { close(initialSync) })
		}
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	go func() { defer close(watchDone); _ = Watch(ctx, c.Client, m, notify) }()
	defer func() { cancel(); <-watchDone }()
	var editor Editor
	defer editor.Clear()
	u := UIState{cache: &bodyCache{}}
	if c.Theme != nil {
		u.Theme = *c.Theme
	}
	var retry *inbox.Input
	results := make(chan commandResult, 1)
	var cancelJob context.CancelFunc
	canceledWaiting := false
	start := func(outbox string, fn func(context.Context) commandResult) {
		u.Busy = true
		u.Outbox = outbox
		u.Notification = Notification{}
		canceledWaiting = false
		jobCtx, stop := context.WithCancel(ctx)
		cancelJob = stop
		go func() {
			defer stop()
			r := fn(jobCtx)
			select {
			case results <- r:
			case <-ctx.Done():
			}
		}()
	}
	send := func(input inbox.Input) {
		u.Scroll = 0
		u.Transcript = nil
		generation := m.Snapshot().Generation
		var text string
		if input.Kind == inbox.InputExternal {
			_ = json.Unmarshal(input.Payload, &text)
		} else {
			text = "stop request"
		}
		start("sending  "+text, func(ctx context.Context) commandResult {
			_, err := c.Client.Submit(ctx, c.ID, generation, input)
			return commandResult{err: err, input: &input}
		})
	}
	notice := func(text, kind string) {
		u.Notification = Notification{Text: SafeText(text), Kind: kind}
		if before, after, ok := strings.Cut(text, "; /child-retry"); ok {
			u.Notification.Text = SafeText(before)
			u.Notification.Hint = "/child-retry" + SafeText(after)
		}
		if kind == "" {
			u.Notification.Until = time.Now().Add(8 * time.Second)
		}
	}
	stopSession := func(mode inbox.ControlMode) {
		payload, _ := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: "terminal user requested stop"})
		send(inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
	}
	var renderer Renderer
	var complete completion
	complete.reset()
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	dirty := true
	width, height := 80, 24
	previousWidth, previousHeight, previousRows := 0, 0, 0
	previousConversationHeight := 0
	previousStreaming := false
	previousView := ViewChat
	var frame Frame
	for {
		if c.Size != nil {
			w, h := c.Size()
			if w != width || h != height {
				width, height = w, h
				dirty = true
			}
		}
		if dirty {
			u.Now = time.Now()
			u.Input = editor.Text()
			u.Cursor = editor.cursor
			u.Pasted = editor.Pasted
			if u.Picker != nil && u.Picker.Kind == "view" {
				p := u.Picker
				if (len(p.Views) == 3) != (width >= 120) {
					selected := p.Views[p.Selection]
					u.Picker = viewPicker(selected, width)
				}
			}
			if c.Viewer != nil {
				u.Viewer = c.Viewer(u.Now)
			}
			if u.Sheet != nil && u.Sheet.Analysis != "" {
				u.Sheet.Lines = analysis.Lines(m.Analysis(u.Now, u.Viewer.Rows), u.Sheet.Analysis, u.Theme.ASCII)
			}
			if c.Panel != nil {
				u.LegacyPanel = c.Panel(width, height)
			}
			s := m.Snapshot()
			streaming := s.Progress != nil && s.Progress.Mode == "streaming" && !s.Progress.Done
			frame = RenderFrame(s, u, width, height)
			if c.ViewChanged != nil && frame.View != previousView {
				c.ViewChanged(frame.View)
			}
			if u.Picker != nil {
				u.Picker.Offset = frame.PickerOffset
			}
			if u.Sheet != nil {
				u.Sheet.Offset = frame.SheetOffset
			}
			if frame.View != ViewOrchestration && previousView == frame.View && u.Scroll > 0 && previousWidth == width && previousHeight == height && (previousRows != frame.ConversationRows || previousStreaming != streaming) {
				// Preserve the first visible canonical row when incoming data or
				// the streaming pulse changes the available transcript height.
				// Resize keeps the user's bottom-relative offset instead.
				u.Scroll = max(0, u.Scroll+frame.ConversationRows-previousRows-frame.ConversationHeight+previousConversationHeight)
				frame = RenderFrame(s, u, width, height)
			}
			if frame.View != ViewOrchestration {
				u.Scroll = min(u.Scroll, frame.ScrollMax)
			}
			if frame.View == ViewOrchestration {
				u.OrchestrationScroll = frame.OrchestrationOffset
			}
			u.MenuOffset = frame.MenuOffset
			previousWidth, previousHeight, previousRows = width, height, frame.ConversationRows
			previousConversationHeight, previousStreaming = frame.ConversationHeight, streaming
			previousView = frame.View
			if _, err := io.WriteString(c.Output, renderer.Draw(frame)); err != nil {
				return err
			}
			dirty = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			if !u.Notification.Until.IsZero() && !now.Before(u.Notification.Until) {
				u.Notification = Notification{}
				dirty = true
			}
			s := m.Snapshot()
			if now.Unix() != u.Now.Unix() || !u.Theme.Still && now.UnixMilli()/120 != u.Now.UnixMilli()/120 && (u.Busy || s.Progress != nil || !s.Connected || pulse(s, u, makeLayout(width, height)) != nil) {
				dirty = true
			}
		case <-changes:
			dirty = true
		case _, ok := <-c.Updates:
			if !ok {
				c.Updates = nil
			}
			dirty = true
		case result := <-results:
			u.Busy = false
			u.Outbox = ""
			cancelJob = nil
			if result.input != nil {
				if result.err != nil {
					retry = result.input
				} else {
					retry = nil
				}
			}
			if !canceledWaiting {
				if result.err != nil {
					text := "not confirmed: " + result.err.Error()
					var exportErr *ExportError
					if errors.As(result.err, &exportErr) {
						text = "export failed: " + exportErr.Error()
					}
					notice(text, "error")
					if result.input != nil {
						u.Notification.Hint = "/retry resends it with the same input ID; it cannot be committed twice"
					}
				} else if result.transcript != nil {
					u.Transcript = result.transcript
					if len(result.transcript.Entries) == 0 && !result.transcript.Newer {
						u.Transcript = nil
					}
					u.Scroll = 0
					if result.transcriptTop && u.Transcript != nil {
						u.Scroll = RenderFrame(m.Snapshot(), u, width, height).ScrollMax
					}
					previousWidth = -1
					u.Notification = Notification{}
				} else if result.picker != nil {
					u.Picker = result.picker
					u.Sheet = nil
					u.Notification = Notification{}
				} else if result.sheet != nil {
					u.Sheet = result.sheet
					u.Notification = Notification{}
				} else if result.text != "" {
					notice(result.text, result.kind)
				} else {
					u.Notification = Notification{}
				}
			}
			dirty = true
		case key, ok := <-c.Keys:
			if !ok || key.Name == "detach" {
				return nil
			}
			dirty = true
			s := m.Snapshot()
			u.Input, u.Pasted = editor.Text(), editor.Pasted
			if !key.Paste && u.Private == nil && u.Sheet != nil && u.Sheet.Analysis == "" {
				if key.Name == "escape" {
					u.Sheet = nil
					continue
				}
				if key.Name == "pageup" || key.Name == "pagedown" {
					delta := max(1, frame.SheetHeight)
					if key.Name == "pageup" {
						delta = -delta
					}
					u.Sheet.Offset = max(0, u.Sheet.Offset+delta)
					continue
				}
			}
			if !key.Paste && u.Private == nil && u.Sheet != nil && u.Sheet.Analysis != "" && !u.Busy {
				switch key.Name {
				case "escape", "enter":
					u.Picker = analyzePicker(u.Sheet.Analysis)
					u.Sheet = nil
					continue
				case "pageup", "pagedown", "up", "down":
					delta := max(1, height/3-1)
					if key.Name == "up" || key.Name == "down" {
						delta = 1
					}
					if key.Name == "pageup" || key.Name == "up" {
						delta = -delta
					}
					u.Sheet.Offset = max(0, u.Sheet.Offset+delta)
					continue
				}
			}
			if !key.Paste && u.Private == nil && u.Picker != nil && !u.Busy {
				p := u.Picker
				switch key.Name {
				case "escape":
					if p.Kind == "effort" {
						var current *sessionstore.RuntimeSelection
						if s.Selection != nil && s.Selection.Provider == p.Provider {
							current = s.Selection
						}
						u.Picker = modelPicker(p.Catalog, current, p.ToolBridge)
						u.Picker.Revision = p.Revision
						u.Picker.Provider = p.Provider
						u.Picker.Providers = p.Providers
						u.Picker.Hint = p.Hint
					} else if p.Kind == "model" && len(p.Providers) > 1 {
						u.Picker = providerPicker(p.Providers, s.Selection)
						u.Picker.Revision = p.Revision
					} else if p.Kind == "export" {
						u.Picker = analyzePicker("Export")
					} else {
						u.Picker = nil
					}
					continue
				case "up", "down", "tab":
					delta := 1
					if key.Name == "up" {
						delta = -1
					}
					p.move(delta)
					continue
				case "pageup", "pagedown":
					delta := max(1, height/3-2)
					if key.Name == "pageup" {
						delta = -delta
					}
					p.move(delta)
					continue
				case "enter":
					if len(p.Options) == 0 {
						continue
					}
					if p.Kind == "view" {
						chosen := p.Views[p.Selection]
						if chosen == ViewSplit && width < 120 {
							notice("Split requires 120 columns", "warning")
							continue
						}
						u.View, u.Picker = chosen, nil
						previousWidth = -1
						continue
					}
					if p.Kind == "provider" {
						b := p.Providers[p.Selection]
						if b.Availability != "available" {
							notice("provider "+b.Availability, "warning")
							continue
						}
						current := s.Selection
						start("loading "+b.Name+" catalog", func(ctx context.Context) commandResult {
							catalog := b.Catalog
							if c.ProviderCatalog != nil {
								var e error
								catalog, e = c.ProviderCatalog(ctx, b.ID, false)
								if e != nil {
									return commandResult{err: e}
								}
							}
							var selected *sessionstore.RuntimeSelection
							if current != nil && current.Provider == b.ID {
								selected = current
							}
							next := modelPicker(catalog, selected, b.Tools)
							next.Provider = b.ID
							next.capability(modelcatalog.Capabilities{Tools: b.Tools, ToolBridge: b.ToolBridge})
							next.Providers = p.Providers
							next.Revision = p.Revision
							next.Title = b.Name + " · Model"
							return commandResult{picker: next}
						})
						continue
					}
					if p.Kind == "model" {
						u.Picker = p.effort(s.Selection)
						continue
					}
					if p.Kind == "analyze" {
						view := p.Options[p.Selection]
						if view == "Export" {
							u.Picker = &Picker{Kind: "export", Title: "Export · metadata/statistics only", Options: []string{"JSON", "Markdown"}}
						} else {
							u.Sheet = &Sheet{Title: "Analysis · " + view, Analysis: view}
							u.Picker = nil
						}
						continue
					}
					if p.Kind == "export" {
						if c.ExportAnalysis == nil {
							notice("analysis export unavailable", "warning")
							continue
						}
						report := m.Analysis(time.Now(), u.Viewer.Rows)
						format := p.Options[p.Selection]
						u.Picker = nil
						start("exporting metadata/statistics only", func(ctx context.Context) commandResult {
							path, err := c.ExportAnalysis(ctx, report, format)
							return commandResult{text: "Analysis exported: " + path, err: err}
						})
						continue
					}
					if p.Kind == "response-export" {
						choice := p.Responses[p.Selection]
						u.Picker = nil
						start("exporting canonical agent response", func(ctx context.Context) commandResult {
							path, err := ExportResponse(ctx, c.Client, c.ID, choice, c.ExportDirectory)
							return commandResult{text: "exported to " + path, err: err}
						})
						continue
					}
					if p.Kind == "effort" {
						if c.SelectModel == nil || s.Selection == nil {
							notice("model switching unavailable", "warning")
							continue
						}
						selected := sessionstore.RuntimeSelection{Version: 1, RequestID: uuid.New().String(), Provider: p.Provider, Model: p.Model.ID, Name: p.Model.Name, Effort: p.selectedEffort(), ContextWindow: p.Model.ContextWindow}
						generation, revision := s.Generation, p.Revision
						u.Picker = nil
						start("changing model for the next turn", func(ctx context.Context) commandResult {
							return commandResult{text: "Model changed: " + selectionLabel(selected, u.Theme) + "; applies from the next completed turn boundary", err: c.SelectModel(ctx, generation, revision, selected)}
						})
						continue
					}
				}
			}
			if key.Name == "cancel" {
				if u.Private != nil {
					editor.Clear()
					u.Private = nil
					notice("key entry canceled", "")
					complete.resetMenu(&u)
					continue
				}
				if u.Busy {
					if cancelJob != nil {
						cancelJob()
					}
					canceledWaiting = true
					u.Outbox = ""
					notice("stopped waiting; the input may already be committed and will appear above if so", "warning")
					continue
				}
				editor.Clear()
				complete.resetMenu(&u)
				u.Sheet = nil
				u.Picker = nil
				u.Scroll = 0
				u.Transcript = nil
				stopSession(inbox.StopHard)
				continue
			}
			if key.Name == "pageup" {
				if u.View.effective(width) == ViewOrchestration {
					u.OrchestrationScroll = max(0, u.OrchestrationScroll-max(1, frame.OrchestrationHeight))
					continue
				}
				step := max(1, frame.ConversationHeight)
				older, boundary := s.OlderDropped, sessionstore.Sequence(0)
				if len(s.Entries) > 0 {
					boundary = s.Entries[0].Sequence
				}
				if u.Transcript != nil {
					older, boundary = u.Transcript.Older, u.Transcript.First
				}
				if u.Scroll >= frame.ScrollMax && older && boundary > 1 && !u.Busy && u.Private == nil {
					start("loading earlier canonical history", func(ctx context.Context) commandResult {
						page, err := loadTranscriptPage(ctx, c.Client, c.ID, boundary, s.After, true)
						return commandResult{transcript: page, err: err}
					})
				} else {
					u.Scroll = min(frame.ScrollMax, u.Scroll+step)
				}
				continue
			}
			if key.Name == "pagedown" {
				if u.View.effective(width) == ViewOrchestration {
					u.OrchestrationScroll = min(frame.OrchestrationMax, u.OrchestrationScroll+max(1, frame.OrchestrationHeight))
					continue
				}
				if u.Scroll == 0 && u.Transcript != nil && u.Transcript.Last < s.After && !u.Busy && u.Private == nil {
					boundary := u.Transcript.Last
					start("loading later canonical history", func(ctx context.Context) commandResult {
						page, err := loadTranscriptPage(ctx, c.Client, c.ID, boundary, s.After, false)
						return commandResult{transcript: page, transcriptTop: true, err: err}
					})
				} else {
					u.Scroll = max(0, u.Scroll-max(1, frame.ConversationHeight))
					if u.Scroll == 0 && u.Transcript != nil && u.Transcript.Last >= s.After {
						u.Transcript = nil
						previousWidth = -1
					}
				}
				continue
			}
			if key.Name == "up" || key.Name == "down" {
				if !key.Paste {
					delta := 1
					if key.Name == "up" {
						delta = -1
					}
					complete.move(&editor, s, &u, delta)
				}
				continue
			}
			if key.Name == "tab" {
				complete.apply(&editor, s, &u)
				continue
			}
			if key.Name != "enter" {
				before, pasted := editor.Text(), editor.Pasted
				editor.Apply(key)
				if editor.Text() != before || editor.Pasted != pasted {
					complete.resetMenu(&u)
				}
				continue
			}
			if u.Busy {
				notice("request pending; ^C cancels waiting", "warning")
				continue
			}
			u.Notification = Notification{}
			if u.Sheet != nil {
				u.Sheet = nil
				complete.resetMenu(&u)
				continue
			}
			if candidate, ok := selectedCommand(s, u); ok {
				if candidate.Args == requiredArguments {
					editor.replace(commandPrefix(candidate))
					complete.resetMenu(&u)
					continue
				}
				editor.replace(candidate.Name)
			}
			complete.resetMenu(&u)
			line := editor.Text()
			editor.Clear()
			if u.Private != nil {
				ref := *u.Private
				u.Private = nil
				secret := credential.NewSecret(line)
				line = ""
				start("storing key for "+ref.Provider+"/"+ref.ID, func(ctx context.Context) commandResult {
					_, err := c.Client.Login(ctx, ref, secret)
					secret = credential.Secret{}
					if err != nil {
						err = errors.New("credential storage failed")
					}
					return commandResult{text: "API key stored; it is checked on the next provider request", err: err}
				})
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			command, registered := submittedCommand(line)
			if !registered {
				u.Scroll = 0
				u.Transcript = nil
				payload, _ := json.Marshal(line)
				send(inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputExternal, Payload: payload})
				continue
			}
			line = command
			parts := strings.Fields(line)
			switch parts[0] {
			case "/view":
				if len(parts) == 1 {
					u.Picker, u.Sheet = viewPicker(u.View, width), nil
					continue
				}
				if len(parts) != 2 || parts[1] != string(ViewChat) && parts[1] != string(ViewOrchestration) && parts[1] != string(ViewSplit) {
					notice("usage: /view [chat | orchestration | split]", "warning")
					continue
				}
				chosen := ViewMode(parts[1])
				if chosen == ViewSplit && width < 120 {
					notice("Split requires 120 columns; use /view chat or /view orchestration", "warning")
					continue
				}
				u.View, u.Sheet, u.Picker = chosen, nil, nil
				previousWidth = -1
			case "/export":
				if len(parts) > 2 || len(parts) == 2 && parts[1] != "last" && parts[1] != "all" {
					notice("usage: /export [last | all]", "warning")
					continue
				}
				through := s.After
				if len(parts) == 1 {
					start("loading exportable canonical responses", func(ctx context.Context) commandResult {
						through, err := exportBoundary(ctx, c.Client, c.ID, through)
						if err != nil {
							return commandResult{err: err}
						}
						choices, err := ResponseChoices(ctx, c.Client, c.ID, through)
						return commandResult{picker: responseExportPicker(choices), err: err}
					})
					continue
				}
				all := parts[1] == "all"
				start("exporting canonical public conversation", func(ctx context.Context) commandResult {
					through, err := exportBoundary(ctx, c.Client, c.ID, through)
					if err != nil {
						return commandResult{err: err}
					}
					var path string
					if all {
						path, err = ExportConversation(ctx, c.Client, c.ID, through, c.ExportDirectory)
					} else {
						path, err = ExportLastResponse(ctx, c.Client, c.ID, through, c.ExportDirectory)
					}
					return commandResult{text: "exported to " + path, err: err}
				})
			case "/help":
				u.Sheet = helpSheet(u.Theme, height < 20)
			case "/model":
				if len(parts) > 2 || len(parts) == 2 && parts[1] != "refresh" {
					notice("usage: /model [refresh]", "warning")
					continue
				}
				current := s.Selection
				fetch := c.ModelCatalog
				if len(parts) == 2 && c.RefreshModelCatalog != nil {
					fetch = c.RefreshModelCatalog
				}
				start("loading model catalog", func(ctx context.Context) commandResult {
					selectionSnapshot := s
					if current == nil {
						// A startup command can precede the first subscription.
						// Wait for its canonical namespace rather than inventing one.
						select {
						case <-initialSync:
						case <-ctx.Done():
							return commandResult{err: ctx.Err()}
						}
						selectionSnapshot = m.Snapshot()
						current = selectionSnapshot.Selection
					}
					catalog := modelcatalog.Catalog{Problem: "catalog unavailable"}
					if fetch != nil {
						var err error
						catalog, err = fetch(ctx)
						if err != nil {
							catalog = modelcatalog.Catalog{Problem: "catalog unavailable"}
						}
					}
					p := modelPicker(catalog, current, m.Analysis(time.Now(), nil).ToolBridgeEnabled)
					if c.ProviderCatalogs != nil {
						providers, e := c.ProviderCatalogs(ctx)
						if e != nil {
							return commandResult{err: e}
						}
						if len(providers) > 1 {
							p = providerPicker(providers, current)
						} else if len(providers) == 1 {
							p = modelPicker(catalog, current, providers[0].Tools)
							p.capability(modelcatalog.Capabilities{Tools: providers[0].Tools, ToolBridge: providers[0].ToolBridge})
						}
					}
					if selectionSnapshot.PendingSelection != nil {
						p.Revision = selectionSnapshot.PendingSelection.Revision
					}
					return commandResult{picker: p}
				})
			case "/analyze":
				u.Picker = analyzePicker("")
			case "/detach":
				return nil
			case "/retry":
				if retry != nil {
					send(*retry)
				} else {
					notice("no uncertain input to retry", "")
				}
			case "/resume":
				start("resuming this session", func(ctx context.Context) commandResult {
					_, err := c.Client.Open(ctx, host.Resume, c.ID)
					return commandResult{text: "session resumed", err: err}
				})
			case "/stop":
				mode := inbox.StopHard
				if len(parts) > 1 && parts[1] == "idle" {
					mode = inbox.StopWhenIdle
				}
				stopSession(mode)
			case "/login":
				if len(parts) != 3 {
					notice("usage: /login PROVIDER ID (then enter key privately)", "warning")
					continue
				}
				ref := credential.Reference{Provider: parts[1], ID: parts[2], Method: credential.APIKey}
				if err := authflow.ValidateMethod(ref); err != nil {
					notice(err.Error(), "warning")
					continue
				}
				u.Private = &ref
			case "/logout":
				if len(parts) != 3 {
					notice("usage: /logout PROVIDER ID", "warning")
					continue
				}
				ref := credential.Reference{Provider: parts[1], ID: parts[2], Method: credential.APIKey}
				start("removing "+ref.Provider+"/"+ref.ID, func(ctx context.Context) commandResult {
					return commandResult{text: "credential removed", err: c.Client.Logout(ctx, ref)}
				})
			case "/methods":
				start("loading authentication methods", func(ctx context.Context) commandResult {
					methods, err := c.Client.Methods(ctx)
					sheet := &Sheet{Title: "auth"}
					for _, m := range methods {
						sheet.Lines = append(sheet.Lines, fmt.Sprintf("%s/%s   %s", m.Provider, m.Method, m.Status))
					}
					return commandResult{sheet: sheet, err: err}
				})
			case "/credentials":
				start("loading credential references", func(ctx context.Context) commandResult {
					entries, err := c.Client.ListCredentials(ctx)
					sheet := &Sheet{Title: "auth"}
					for _, e := range entries {
						sheet.Lines = append(sheet.Lines, e.Reference.Provider+"/"+e.Reference.ID)
					}
					if len(sheet.Lines) == 0 {
						sheet.Lines = []string{"no stored credentials"}
					}
					return commandResult{sheet: sheet, err: err}
				})
			default:
				if c.Command == nil {
					notice("unknown command "+parts[0]+"; type / or /help", "warning")
					continue
				}
				start("request pending", func(ctx context.Context) commandResult {
					text, ok := c.Command(ctx, line)
					kind := ""
					if !ok {
						text = "unknown command " + parts[0] + "; type / or /help"
						kind = "warning"
					} else if strings.Contains(text, "request failed:") {
						kind = "error"
					} else if strings.HasPrefix(text, "usage:") || strings.HasPrefix(text, "unknown") || strings.HasPrefix(text, "invalid") {
						kind = "warning"
					}
					return commandResult{text: text, kind: kind}
				})
			}
		}
	}
}
