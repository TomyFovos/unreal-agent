package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
	"github.com/unreallabsai/unreal-agent/internal/usertext"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	ledgerusage "github.com/unreallabsai/unreal-agent/harness/usage"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type Client interface {
	Reader
	Open(context.Context, host.Mode, session.ID) (host.View, error)
	Submit(context.Context, session.ID, string, inbox.Input) (host.Receipt, error)
}
type permissionWatchResult struct {
	generation   string
	connection   permission.ControlConnection
	snapshot     *permission.ModeSnapshot
	disconnected bool
}

type Config struct {
	WatchPermissions func(context.Context, string) (permission.ControlConnection, error)
	Client           Client
	ID               session.ID
	Keys             <-chan Key
	Output           io.Writer
	Size             func() (int, int)
	Resize           <-chan os.Signal
	Theme            *Theme
	Viewer           func(time.Time) viewer.PanelSnapshot
	ViewChanged      func(ViewMode) // frontend read-only observation scope
	// ViewSelected reports the view the person chose with /view, for reuse
	// when the Session is opened again. Narrow fallbacks and exit do not call it.
	ViewSelected func(ViewMode)
	// InitialView is the first view; empty means Split, which narrow
	// terminals show as Chat until they reach 120 columns.
	InitialView         ViewMode
	Updates             <-chan struct{}
	Command             func(context.Context, string) (string, bool)
	ModelCatalog        func(context.Context) (modelcatalog.Catalog, error)
	RefreshModelCatalog func(context.Context) (modelcatalog.Catalog, error)
	SelectModel         func(context.Context, string, uint64, sessionstore.RuntimeSelection) error
	ProviderCatalogs    func(context.Context) ([]modelcatalog.Provider, error)
	ProviderCatalog     func(context.Context, string, bool) (modelcatalog.Catalog, error)
	RuntimeCapabilities func(context.Context) (map[string]modelcatalog.Capabilities, error)
	ExportAnalysis      func(context.Context, analysis.Report, string) (string, error)
	AnalysisInspect     analysis.InspectFunc
	UsageTree           func(context.Context, time.Duration) (subagent.UsageReport, error)
	// Empty uses the standard private state export directory. No subprocess
	// backend is used for public conversation exports.
	ExportDirectory string
	// OpenCodeGoKeyCommand is the shell command /login shows for saving the
	// OpenCode Go key; empty shows only the guidance.
	OpenCodeGoKeyCommand string
}
type commandResult struct {
	text, kind           string
	errorProvider        string
	permissionSnapshot   *permission.ModeSnapshot
	permissionGeneration string
	err                  error
	input                *inbox.Input
	sheet                *Sheet
	picker               *Picker
	capabilities         map[string]modelcatalog.Capabilities
	transcript           *transcriptPage
	transcriptTop        bool
	olderLoaded          bool
	olderStep            int
	// clipboard is text for the terminal's clipboard (OSC 52), written by the
	// render loop so it never interleaves with a frame.
	clipboard string
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
	inspectAnalysis := c.AnalysisInspect
	if inspectAnalysis == nil {
		inspectAnalysis = rootAnalysisInspector(c.Client, c.ID)
	}
	var current Snapshot
	snapshot := func() Snapshot {
		if current.Version != m.Version() {
			current = m.Snapshot()
		}
		return current
	}
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
	u := UIState{cache: &bodyCache{}, View: ViewSplit}
	if c.InitialView != "" {
		u.View = c.InitialView
	}
	chosenView := u.View
	files := newFileCompletion(ctx)
	defer files.close(&u)
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
		u.HistoryTop = false
		u.Transcript = nil
		generation := snapshot().Generation
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
	exportAnalysis := func(format string) {
		p := u.analysis
		if c.ExportAnalysis == nil {
			p.problem = "Analysis export unavailable"
			return
		}
		if p.report == nil {
			p.exportFormat = format
			return
		}
		report := *p.report
		p.cancel()
		u.analysis = nil
		start("exporting metadata/statistics only", func(ctx context.Context) commandResult {
			path, err := c.ExportAnalysis(ctx, report, format)
			return commandResult{text: "Analysis exported: " + path, err: err}
		})
	}
	stopSession := func(modes ...inbox.ControlMode) {
		inputs := make([]inbox.Input, len(modes))
		for i, mode := range modes {
			payload, _ := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: "terminal user requested stop"})
			inputs[i] = inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload}
		}
		if len(inputs) == 1 {
			send(inputs[0])
			return
		}
		generation := snapshot().Generation
		start("sending  stop request", func(ctx context.Context) commandResult {
			for _, input := range inputs {
				if _, err := c.Client.Submit(ctx, c.ID, generation, input); err != nil {
					return commandResult{err: err}
				}
			}
			return commandResult{}
		})
	}
	var renderer Renderer
	var complete completion
	complete.reset()
	clock := time.NewTimer(time.Hour)
	defer clock.Stop()
	var tick, sizePoll <-chan time.Time
	if c.Size != nil && c.Resize == nil {
		// Embedded callers with only a size callback can still report resizes.
		poll := time.NewTicker(120 * time.Millisecond)
		defer poll.Stop()
		sizePoll = poll.C
	}
	dirty := true
	viewerDirty := true
	viewerSecond := int64(0)
	analysisUpdates := make(chan analysisUpdate, 1)
	spendUpdates := make(chan []ledgerusage.Spend, 1)
	if c.UsageTree != nil {
		go watchSpend(ctx, c.UsageTree, spendUpdates)
	}
	width, height := 80, 24
	previousWidth, previousHeight, previousRows := 0, 0, 0
	previousConversationHeight := 0
	previousStreaming := false
	previousView := ViewChat
	var frame Frame
	// queued is a key the loop handles before reading the next real one: a
	// command picker's choice is typed into the editor and submitted.
	queued := make(chan Key, 1)
	var permissionConnection permission.ControlConnection
	var permissionGeneration string
	var selectionGeneration string
	var selectionRevision uint64
	var cancelPermission context.CancelFunc
	permissionEvents := make(chan permissionWatchResult, 2)
	defer func() {
		if cancelPermission != nil {
			cancelPermission()
		}
		if permissionConnection.Cancel != nil {
			permissionConnection.Cancel()
		}
	}()
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
			if !u.Notification.Until.IsZero() && !u.Now.Before(u.Notification.Until) {
				u.Notification = Notification{}
			}
			u.Input = editor.Text()
			u.Cursor = editor.cursor
			u.Selected = editor.Selected
			u.Pasted = editor.Pasted
			if u.Picker != nil && u.Picker.Kind == "view" {
				p := u.Picker
				if (len(p.Views) == 3) != (width >= 120) {
					selected := p.Views[p.Selection]
					u.Picker = viewPicker(selected, width)
				}
			}
			if c.Viewer != nil && (viewerDirty || viewerSecond != u.Now.Unix()) {
				u.Viewer = c.Viewer(u.Now)
				viewerDirty, viewerSecond = false, u.Now.Unix()
				u.Parent = nil
				for i := range u.Viewer.Rows {
					if u.Viewer.Rows[i].ID == c.ID {
						u.Parent = &u.Viewer.Rows[i]
						break
					}
				}
			}
			s := snapshot()
			if s.Connected && s.Selection != nil && !u.Busy {
				if s.Generation == selectionGeneration && s.Selection.Revision > selectionRevision {
					notice("モデルを切り替えました  "+selectionLabel(*s.Selection, u.Theme), "")
				}
				selectionGeneration, selectionRevision = s.Generation, s.Selection.Revision
			}
			if u.analysis != nil {
				u.analysis.request(m, inspectAnalysis, c.UsageTree, s, u.Viewer, u.Theme, u.Now, makeLayout(width, height).width, analysisUpdates)
			}
			if c.WatchPermissions != nil && s.Connected && s.Generation != "" && permissionGeneration != s.Generation {
				if cancelPermission != nil {
					cancelPermission()
				}
				if permissionConnection.Cancel != nil {
					permissionConnection.Cancel()
				}
				permissionConnection = permission.ControlConnection{}
				permissionGeneration = s.Generation
				watchCtx, stop := context.WithCancel(ctx)
				cancelPermission = stop
				generation := s.Generation
				go func() {
					conn, err := c.WatchPermissions(watchCtx, generation)
					if err != nil {
						return
					}
					select {
					case permissionEvents <- permissionWatchResult{generation: generation, connection: conn}:
					case <-watchCtx.Done():
						conn.Cancel()
						return
					}
					defer conn.Cancel()
					for {
						select {
						case snapshot, ok := <-conn.Updates:
							if !ok {
								if watchCtx.Err() == nil {
									select {
									case permissionEvents <- permissionWatchResult{generation: generation, disconnected: true}:
									case <-watchCtx.Done():
									}
								}
								return
							}
							select {
							case permissionEvents <- permissionWatchResult{generation: generation, snapshot: &snapshot}:
							case <-watchCtx.Done():
								return
							}
						case <-watchCtx.Done():
							return
						}
					}
				}()
			}
			if u.analysis == nil && s.Permissions.Enabled && len(s.Permissions.Pending) > 0 && s.Permissions.Pending[0].ID != u.PermissionDismissed && u.Picker == nil && u.Sheet == nil && !u.Busy && strings.TrimSpace(editor.Text()) == "" {
				u.Picker = operationPermissionConfirmation(s, s.Permissions.Pending[0])
			}
			files.sync(s, &u, &editor)
			streaming := s.Progress != nil && s.Progress.Mode == "streaming" && !s.Progress.Done
			frame = RenderFrame(s, u, width, height)
			if u.analysis == nil && u.HistoryTop && frame.View != ViewOrchestration {
				u.Scroll = frame.ScrollMax
				frame = RenderFrame(s, u, width, height)
			}
			if c.ViewChanged != nil && frame.View != previousView {
				c.ViewChanged(frame.View)
			}
			if u.View != chosenView {
				chosenView = u.View
				if c.ViewSelected != nil {
					c.ViewSelected(chosenView)
				}
			}
			if u.Picker != nil {
				u.Picker.Offset = frame.PickerOffset
			}
			if u.Sheet != nil {
				u.Sheet.Offset = frame.SheetOffset
			}
			if u.analysis == nil && !u.HistoryTop && frame.View != ViewOrchestration && previousView == frame.View && u.Scroll > 0 && previousWidth == width && previousHeight == height && (previousRows != frame.ConversationRows || previousStreaming != streaming) {
				// Preserve the first visible canonical row when incoming data or
				// the streaming pulse changes the available transcript height.
				// Resize keeps the user's bottom-relative offset instead.
				u.Scroll = max(0, u.Scroll+frame.ConversationRows-previousRows-frame.ConversationHeight+previousConversationHeight)
				frame = RenderFrame(s, u, width, height)
			}
			if u.analysis == nil && frame.View != ViewOrchestration {
				u.Scroll = min(u.Scroll, frame.ScrollMax)
			}
			if u.analysis == nil && frame.View == ViewOrchestration {
				u.OrchestrationScroll = frame.OrchestrationOffset
			}
			u.MenuOffset = frame.MenuOffset
			if u.analysis != nil {
				if frame.SheetHeight > 0 {
					u.analysis.offset = frame.SheetOffset
				}
				if u.analysis.picker != nil {
					u.analysis.picker.Offset = frame.PickerOffset
				}
				previousWidth = -1
			} else {
				previousWidth, previousHeight, previousRows = width, height, frame.ConversationRows
				previousConversationHeight, previousStreaming = frame.ConversationHeight, streaming
			}
			previousView = frame.View
			if output := renderer.Draw(frame); output != "" {
				if _, err := io.WriteString(c.Output, output); err != nil {
					return err
				}
			}
			if deadline := nextRender(s, u, frame.View); deadline.IsZero() {
				clock.Stop()
				tick = nil
			} else {
				clock.Reset(time.Until(deadline))
				tick = clock.C
			}
			dirty = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			dirty = true
		case <-sizePoll:
		case _, ok := <-c.Resize:
			if !ok {
				c.Resize = nil
			}
		case <-changes:
			dirty = true
		case result := <-permissionEvents:
			if result.generation != snapshot().Generation {
				if result.connection.Cancel != nil {
					result.connection.Cancel()
				}
				continue
			}
			if result.disconnected {
				permissionGeneration = ""
				permissionConnection = permission.ControlConnection{}
				dirty = true
				continue
			}
			if result.snapshot != nil {
				m.setPermissions(result.generation, *result.snapshot)
			} else {
				permissionConnection = result.connection
				m.setPermissions(result.generation, result.connection.Initial)
			}
			dirty = true
		case result := <-files.events:
			// Revalidate scope/cursor before accepting asynchronous candidates.
			files.sync(snapshot(), &u, &editor)
			files.apply(result)
			dirty = true
		case _, ok := <-c.Updates:
			viewerDirty = true
			if !ok {
				c.Updates = nil
			}
			dirty = true
		case spend := <-spendUpdates:
			u.Spend = spend
			dirty = true
		case result := <-analysisUpdates:
			if u.analysis == result.screen {
				p := u.analysis
				p.accept(result, time.Now())
				if p.report != nil && p.exportFormat != "" {
					exportAnalysis(p.exportFormat)
				}
				dirty = true
			}
		case result := <-results:
			u.HistoryLoading = false
			u.Busy = false
			if result.capabilities != nil {
				m.setCapabilities(result.capabilities)
			}
			u.Outbox = ""
			cancelJob = nil
			if result.permissionGeneration != "" && result.permissionGeneration != snapshot().Generation {
				dirty = true
				continue
			}
			if result.permissionSnapshot != nil {
				m.setPermissions(result.permissionGeneration, *result.permissionSnapshot)
			}
			if result.input != nil {
				if result.err != nil {
					retry = result.input
				} else {
					retry = nil
				}
			}
			if !canceledWaiting {
				if result.err != nil {
					m.diagnostic(result.err)
					provider := ""
					if selected := snapshot().Selection; selected != nil {
						provider = selected.Provider
					}
					if result.errorProvider != "" {
						provider = result.errorProvider
					}
					text := usertext.Failure("", result.err.Error(), provider)
					var exportErr *ExportError
					if errors.As(result.err, &exportErr) {
						text = "export failed: " + exportErr.Error()
					}
					notice(text, "error")
					if result.input != nil {
						u.Notification.Hint = "/retry で安全に再送できます（重複して送信されません）"
					}
				} else if result.olderLoaded {
					u.Scroll += result.olderStep
					u.HistoryTop = result.transcriptTop
					if u.HistoryTop {
						u.Scroll = RenderFrame(snapshot(), u, width, height).ScrollMax
					}
					previousWidth = -1
					u.Notification = Notification{}
				} else if result.transcript != nil {
					u.Transcript = result.transcript
					if len(result.transcript.Entries) == 0 && !result.transcript.Newer {
						u.Transcript = nil
					}
					u.Scroll = 0
					u.HistoryTop = result.transcriptTop && u.Transcript != nil
					if result.transcriptTop && u.Transcript != nil {
						u.Scroll = RenderFrame(snapshot(), u, width, height).ScrollMax
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
				} else if result.clipboard != "" {
					if err := writeClipboard(c.Output, result.clipboard); err != nil {
						return err
					}
					notice(result.text, result.kind)
				} else if result.text != "" {
					notice(result.text, result.kind)
				} else {
					u.Notification = Notification{}
				}
			}
			dirty = true
		case key, ok := <-nextKeys(queued, c.Keys):
			if !ok || key.Name == "detach" {
				return nil
			}
			dirty = true
			if p := u.analysis; p != nil {
				format, close := p.key(key, frame)
				switch {
				case close:
					p.cancel()
					u.analysis = nil
				case format != "":
					exportAnalysis(format)
				}
				continue
			}
			s := snapshot()
			u.Input, u.Pasted = editor.Text(), editor.Pasted
			files.sync(s, &u, &editor)
			wheel := key.Name == "wheelup" || key.Name == "wheeldown"
			if wheel {
				// Wheel reports never interact with approval/model/file pickers,
				// private inputs or the composer. Only the hovered history pane
				// scrolls; synthetic keyboard-only tests may omit coordinates.
				if key.Paste || u.Picker != nil || u.Sheet != nil {
					continue
				}
				if key.MouseY != 0 {
					if frame.View == ViewSplit && key.MouseX > frame.ConversationWidth+3 && key.MouseY > 1 && key.MouseY <= 1+frame.OrchestrationHeight {
						delta := 3
						if key.Name == "wheelup" {
							delta = -delta
						}
						u.OrchestrationScroll = min(frame.OrchestrationMax, max(0, u.OrchestrationScroll+delta))
						continue
					}
					if key.MouseX > frame.ConversationWidth || key.MouseY <= frame.ConversationY || key.MouseY > frame.ConversationY+frame.ConversationHeight {
						continue
					}
				}
				if key.Name == "wheelup" {
					key.Name = "pageup"
				} else {
					key.Name = "pagedown"
				}
			}
			// Selection belongs to the composer, including when slash completion is open.
			if (u.Picker == nil || u.Picker.Kind == "file") && u.Sheet == nil && !key.Paste {
				if key.Name == "selectall" || key.Name == "escape" && editor.Selected {
					if u.Picker != nil && u.Picker.Kind == "file" {
						files.dismissed, files.hasDismissed = files.mention, true
						files.close(&u)
					}
					editor.Apply(key)
					complete.resetMenu(&u)
					continue
				}
				if editor.Selected {
					switch key.Name {
					case "up", "down":
						editor.Apply(key)
						continue
					case "pageup", "pagedown", "historyhome", "historyend":
						editor.Selected = false
					}
				}
			}
			if files.key(key, &u, &editor, width, height) {
				continue
			}
			if !key.Paste && u.Sheet != nil {
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
			if !key.Paste && u.Picker != nil && !u.Busy {
				p := u.Picker
				move := func(delta int) {
					if p.Kind == "copy" && p.CopyOlder && delta > 0 && p.Selection+delta >= len(p.Options)-1 {
						// Rendering updates Offset on the event loop. Detach the
						// picker value before the background loader copies it.
						picker := *p
						start("loading older public messages", func(ctx context.Context) commandResult {
							older, err := loadOlderCopyPicker(ctx, c.Client, c.ID, &picker, delta)
							return commandResult{picker: older, err: err}
						})
					} else {
						p.move(delta)
					}
				}
				if p.Kind == "copy" && (key.Text == " " || key.Name == "space") {
					p.toggleCopy()
					continue
				}
				switch key.Name {
				case "escape":
					if p.Kind == "permission-once" {
						u.PermissionDismissed = p.PermissionOperation
					}
					if p.Kind == "fast" {
						u.Picker = p.back
					} else if p.Kind == "effort" {
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
					} else {
						u.Picker = nil
					}
					continue
				case "up", "down", "tab":
					delta := 1
					if key.Name == "up" {
						delta = -1
					}
					move(delta)
					continue
				case "pageup", "pagedown":
					delta := max(1, height/3-2)
					if key.Name == "pageup" {
						delta = -delta
					}
					move(delta)
					continue
				case "enter":
					if len(p.Options) == 0 {
						continue
					}
					commit := func(m modelcatalog.Model, effort llm.ReasoningEffort, fast bool) {
						if c.SelectModel == nil || s.Selection == nil {
							notice("model switching unavailable", "warning")
							return
						}
						selected := sessionstore.RuntimeSelection{Version: 1, RequestID: uuid.New().String(), Provider: p.Provider, Model: m.ID, Name: m.Name, Effort: effort, ContextWindow: m.ContextWindow, Fast: fast}
						generation, revision := s.Generation, p.Revision
						u.Picker = nil
						start("モデルの選択を送信中", func(ctx context.Context) commandResult {
							return commandResult{err: c.SelectModel(ctx, generation, revision, selected)}
						})
					}
					if p.Kind == "copy" {
						u.Picker = nil
						start("copying the selected messages", func(ctx context.Context) commandResult {
							text, err := copyPickerText(ctx, c.Client, c.ID, p)
							return commandResult{clipboard: text, text: fmt.Sprintf("クリップボードに %d 文字コピーしました", utf8.RuneCountInString(text)), err: err}
						})
						continue
					}
					if p.Kind == "permissions" {
						if permissionConnection.Request == nil {
							notice("permission control disconnected; reconnect the TUI", "warning")
							continue
						}
						if p.Selection >= 3 {
							index := p.Selection - 3
							if index >= len(s.Permissions.Pending) {
								notice("approval changed; reopen /permissions", "warning")
								continue
							}
							u.Picker = operationPermissionConfirmation(s, s.Permissions.Pending[index])
							continue
						}
						mode := []permission.Mode{permission.ReadOnly, permission.Ask, permission.FullAccess}[p.Selection]
						request := permissionConnection.Request
						u.Picker = nil
						start("changing Session permissions", func(ctx context.Context) commandResult {
							out, err := request(ctx, permission.ControlRequest{Action: "set_mode", Expected: p.PermissionRevision, Mode: mode})
							return commandResult{permissionGeneration: s.Generation, text: "権限を変更しました: " + mode.Label(), err: err, permissionSnapshot: &out.Snapshot}
						})
						continue
					}
					if p.Kind == "permission-once" {
						if permissionConnection.Request == nil {
							notice("permission control disconnected; reconnect the TUI", "warning")
							continue
						}
						request := permissionConnection.Request
						action := permissionControlAction(p)
						u.Picker = nil
						start("recording human permission decision", func(ctx context.Context) commandResult {
							out, err := request(ctx, action)
							return commandResult{permissionGeneration: s.Generation, text: "Permission decision recorded", err: err, permissionSnapshot: &out.Snapshot}
						})
						continue
					}
					if p.Kind == "command" {
						chosen := p.Commands[p.Selection]
						u.Picker = nil
						editor.replace(chosen)
						queued <- Key{Name: "enter"}
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
						current := s.Selection
						start("loading "+b.Name+" catalog", func(ctx context.Context) commandResult {
							catalog := b.Catalog
							if c.ProviderCatalog != nil {
								var e error
								catalog, e = c.ProviderCatalog(ctx, b.ID, b.Availability != "available")
								if e != nil {
									return commandResult{err: e, errorProvider: b.ID}
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
							result := commandResult{picker: next}
							if c.RuntimeCapabilities != nil {
								result.capabilities, _ = c.RuntimeCapabilities(ctx)
							}
							return result
						})
						continue
					}
					chooseFast := func(m modelcatalog.Model, effort llm.ReasoningEffort) {
						if m.AllowsFast(p.Provider) {
							u.Picker = p.fast(m, effort, s.Selection)
						} else {
							commit(m, effort, false)
						}
					}
					if p.Kind == "model" {
						if chosen := p.Catalog.Models[p.Selection]; chosen.EffortUnknown {
							// The catalog names no Unreal effort: the request
							// carries no reasoning effort, so no effort step.
							chooseFast(chosen, "")
							continue
						}
						u.Picker = p.effort(s.Selection)
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
						chooseFast(p.Model, p.selectedEffort())
						continue
					}
					if p.Kind == "fast" {
						commit(p.Model, p.Effort, p.Selection == 1)
						continue
					}
				}
			}
			if key.Name == "cancel" {
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
				u.HistoryTop = false
				u.Transcript = nil
				stopSession(inbox.StopHard)
				continue
			}
			// Home/End retain composer semantics while editing. Ctrl+Home/End
			// always navigate history, and plain Home/End do so on empty input.
			if !key.Paste && (key.Name == "historyhome" || key.Name == "home" && editor.Text() == "") {
				if u.View.effective(width) == ViewOrchestration {
					u.OrchestrationScroll = 0
					continue
				}
				u.HistoryTop = true
				if s.LazyHistory && s.OlderDropped && !u.Busy {
					u.HistoryLoading = true
					start("古い履歴を読み込み中", func(ctx context.Context) commandResult {
						for {
							if err := m.LoadOlder(ctx, c.Client.(BackwardReader)); err != nil {
								return commandResult{err: err}
							}
							m.mu.Lock()
							older := m.state.OlderDropped
							m.mu.Unlock()
							if !older {
								break
							}
							if err := ctx.Err(); err != nil {
								return commandResult{err: err}
							}
						}
						return commandResult{olderLoaded: true, transcriptTop: true}
					})
				} else if s.OlderDropped && !u.Busy {
					start("loading first canonical history page", func(ctx context.Context) commandResult {
						page, err := loadTranscriptPage(ctx, c.Client, c.ID, 0, s.After, false)
						return commandResult{transcript: page, transcriptTop: true, err: err}
					})
				} else {
					u.Scroll = frame.ScrollMax
				}
				continue
			}
			if !key.Paste && (key.Name == "historyend" || key.Name == "end" && editor.Text() == "") {
				if u.View.effective(width) == ViewOrchestration {
					u.OrchestrationScroll = frame.OrchestrationMax
					continue
				}
				u.Scroll, u.Transcript = 0, nil
				u.HistoryTop = false
				previousWidth = -1
				continue
			}
			// Some terminals synthesize arrows for a wheel in an alternate screen.
			// Empty-composer arrows scroll a line; command completion keeps its keys.
			lineScroll := !key.Paste && editor.Text() == "" && (key.Name == "up" || key.Name == "down")
			if lineScroll {
				if key.Name == "up" {
					key.Name = "pageup"
				} else {
					key.Name = "pagedown"
				}
			}
			scrollStep := max(1, frame.ConversationHeight)
			if wheel {
				scrollStep = 3
			} else if lineScroll {
				scrollStep = 1
			}
			if key.Name == "pageup" {
				u.HistoryTop = false
				if u.View.effective(width) == ViewOrchestration {
					step := max(1, frame.OrchestrationHeight)
					if wheel || lineScroll {
						step = scrollStep
					}
					u.OrchestrationScroll = max(0, u.OrchestrationScroll-step)
					continue
				}
				step := scrollStep
				older, boundary := s.OlderDropped, sessionstore.Sequence(0)
				if len(s.Entries) > 0 {
					boundary = s.Entries[0].Sequence
				}
				if u.Transcript != nil {
					older, boundary = u.Transcript.Older, u.Transcript.First
				}
				if u.Transcript == nil && s.LazyHistory && u.Scroll+step >= frame.ScrollMax && older && !u.Busy {
					u.Scroll = min(frame.ScrollMax, u.Scroll+step)
					u.HistoryLoading = true
					start("古い履歴を読み込み中", func(ctx context.Context) commandResult {
						err := m.LoadOlder(ctx, c.Client.(BackwardReader))
						return commandResult{olderLoaded: true, olderStep: step, err: err}
					})
				} else if u.Scroll >= frame.ScrollMax && older && boundary > 1 && !u.Busy {
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
				u.HistoryTop = false
				if u.View.effective(width) == ViewOrchestration {
					step := max(1, frame.OrchestrationHeight)
					if wheel || lineScroll {
						step = scrollStep
					}
					u.OrchestrationScroll = min(frame.OrchestrationMax, u.OrchestrationScroll+step)
					continue
				}
				if u.Scroll == 0 && u.Transcript != nil && u.Transcript.Last < s.After && !u.Busy {
					boundary := u.Transcript.Last
					start("loading later canonical history", func(ctx context.Context) commandResult {
						page, err := loadTranscriptPage(ctx, c.Client, c.ID, boundary, s.After, false)
						return commandResult{transcript: page, transcriptTop: true, err: err}
					})
				} else {
					u.Scroll = max(0, u.Scroll-scrollStep)
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
			if key.Paste {
				// Files dragged onto the terminal arrive as Windows paths; the
				// sandbox has them in its drop directory once they were sent.
				var missing []string
				key.Text, missing = mapDroppedPaths(key.Text, DropDirectory())
				if len(missing) > 0 {
					notice("not in the sandbox yet: "+strings.Join(missing, ", ")+"; drop them on the sandbox's .cmd in Windows first", "warning")
				}
			}
			if key.Name != "enter" {
				before, pasted := editor.Text(), editor.Pasted
				editor.Apply(key)
				if editor.Text() != before || editor.Pasted != pasted {
					complete.resetMenu(&u)
				}
				continue
			}
			if editor.continuesLine() {
				editor.Apply(Key{Name: "backspace"})
				editor.Apply(Key{Text: "\n"})
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
			line := editor.Expanded()
			editor.Clear()
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
			case "/permissions":
				if len(parts) != 1 {
					notice("usage: /permissions (choose and confirm interactively)", "warning")
					continue
				}
				u.Picker, u.Sheet = permissionPicker(s), nil
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
				if len(parts) > 2 || len(parts) == 2 && parts[1] != "last" && parts[1] != "all" && parts[1] != "pick" {
					notice("usage: /export [last | all | pick]", "warning")
					continue
				}
				if len(parts) == 1 {
					u.Picker, u.Sheet = commandPicker("Export as Markdown", []string{"Latest response", "Full conversation", "Choose a response…"}, []string{"/export last", "/export all", "/export pick"}), nil
					continue
				}
				through := s.After
				if parts[1] == "pick" {
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
			case "/copy":
				if len(parts) > 2 || len(parts) == 2 && parts[1] != "last" && parts[1] != "all" {
					notice("usage: /copy [all | last]", "warning")
					continue
				}
				if len(parts) == 1 {
					u.Sheet = nil
					through := s.After
					start("loading public messages", func(ctx context.Context) commandResult {
						through, err := exportBoundary(ctx, c.Client, c.ID, through)
						if err != nil {
							return commandResult{err: err}
						}
						picker, err := loadCopyPicker(ctx, c.Client, c.ID, through)
						return commandResult{picker: picker, err: err}
					})
					continue
				}
				last := len(parts) == 2 && parts[1] == "last"
				through := s.After
				start("copying the public conversation", func(ctx context.Context) commandResult {
					through, err := exportBoundary(ctx, c.Client, c.ID, through)
					if err != nil {
						return commandResult{err: err}
					}
					var text string
					if last {
						text, err = LastResponseText(ctx, c.Client, c.ID, through)
					} else {
						text, err = ConversationText(ctx, c.Client, c.ID, through)
					}
					if err != nil {
						return commandResult{err: err}
					}
					return commandResult{clipboard: text, text: fmt.Sprintf("クリップボードに %d 文字コピーしました", utf8.RuneCountInString(text))}
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
							m.diagnostic(err)
							catalog = modelcatalog.Catalog{Problem: "catalog unavailable"}
						}
					}
					if !catalog.Available && catalog.ProblemCode != "" {
						m.diagnostic(errors.New("catalog_unavailable: " + catalog.ProblemCode))
					}
					p := modelPicker(catalog, current, m.toolBridgeEnabled())
					if c.ProviderCatalogs != nil {
						providers, e := c.ProviderCatalogs(ctx)
						if e != nil {
							return commandResult{err: e}
						}
						if len(providers) > 1 || current != nil && buildfeatures.Disabled(current.Provider) {
							p = providerPicker(providers, current)
						} else if len(providers) == 1 {
							p = modelPicker(catalog, current, providers[0].Tools)
							p.capability(modelcatalog.Capabilities{Tools: providers[0].Tools, ToolBridge: providers[0].ToolBridge})
						}
					}
					if selectionSnapshot.PendingSelection != nil {
						p.Revision = selectionSnapshot.PendingSelection.Revision
					}
					result := commandResult{picker: p}
					if c.RuntimeCapabilities != nil {
						result.capabilities, _ = c.RuntimeCapabilities(ctx)
					}
					return result
				})
			case "/analyze":
				u.analysis = openAnalysis(ctx)
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
				if len(parts) == 1 {
					p := commandPicker("Stop", []string{"Sub-agents: cancel every child agent", "Main agent: interrupt its current work (sub-agents go on)", "Main agent and sub-agents", "Session: stop now (/resume restarts it)"}, []string{"/stop sub", "/stop main", "/stop main+sub", "/stop session"})
					p.Selection = 1 // interrupting the main agent loses nothing
					u.Picker, u.Sheet = p, nil
					continue
				}
				modes := map[string][]inbox.ControlMode{"sub": {inbox.CancelChildren}, "main": {inbox.InterruptAgent}, "main+sub": {inbox.InterruptAgent, inbox.CancelChildren}, "session": {inbox.StopHard}, "now": {inbox.StopHard}, "idle": {inbox.StopWhenIdle}}
				chosen := modes[parts[len(parts)-1]]
				if len(parts) != 2 || chosen == nil {
					notice("usage: /stop [sub | main | main+sub | session]", "warning")
					continue
				}
				stopSession(chosen...)
			case "/login":
				u.Sheet = &Sheet{Title: "ログイン", Lines: []string{
					"Codex: Bash で codex login を実行してください。",
					"Claude: 別の Bash で claude を起動し、その中で /login を実行してください。",
					"ログイン後は /model refresh、または /model で接続先を選んでください。",
					"",
					"OpenCode Go: OpenCode の管理画面で Go の API キーをコピーし、別の Bash で次を実行してください。キーは画面に出ません。",
				}}
				if !buildfeatures.OpenCodeGoEnabled {
					u.Sheet.Lines = append(u.Sheet.Lines[:3], buildfeatures.OpenCodeGoDisabledMessage)
					continue
				}
				if c.OpenCodeGoKeyCommand != "" {
					u.Sheet.Lines = append(u.Sheet.Lines, "  "+c.OpenCodeGoKeyCommand)
				}
				u.Sheet.Lines = append(u.Sheet.Lines,
					"保存後に /model refresh を実行すると、/model で選べます。",
					"pbpaste がない環境（Linux など）の手順は docs/opencode-go.md にあります。",
					"OpenCode 側で Extra Usage をオフにしてください。オンのままだと、上限を超えた分が従量課金になります。",
				)

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

// nextKeys is where the next key comes from: a queued key before real input.
func nextKeys(queued chan Key, keys <-chan Key) <-chan Key {
	if len(queued) > 0 {
		return queued
	}
	return keys
}
