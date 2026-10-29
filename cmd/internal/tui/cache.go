package tui

import (
	"reflect"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// Wrapping canonical text is expensive compared to painting the visible rows.
// This cache belongs only to a terminal, expires with its bounded entry window,
// and is rebuilt on width/ASCII changes. Input, secrets and progress are excluded.
type bodyKey struct {
	role, text, peer, kind, code string
	clipped, unsafeLinks         bool
}
type cachedBody struct {
	rows []bodyLine
	seen uint64
}
type bodyCache struct {
	width                      int
	ascii                      bool
	frame                      uint64
	entries                    map[bodyKey]cachedBody
	chips                      map[chipKey]cachedChips
	projection                 projectionKey
	index                      *renderIndex
	conversation               conversationCache
	work                       *workCardIndex
	workKey                    projectionKey
	workSecond                 int64
	cards                      []workCard
	beads                      []workBead
	rendered                   frameCache
	headerLine, statisticsLine cachedRegionLine
	attention                  cachedAttention
	cardRows                   map[session.ID]cachedCardRows
}

type regionLineKey struct {
	projection projectionKey
	width      int
	theme      Theme
	clock      frameClock
}
type cachedRegionLine struct {
	key   regionLineKey
	value line
	valid bool
}
type cachedAttention struct {
	key      regionLineKey
	approval bool
	rows     []line
	valid    bool
}

func (c *bodyCache) header(s Snapshot, u UIState, l layout) line {
	k := regionLineKey{projection: renderProjection(s, u), width: l.width, theme: u.Theme, clock: renderClock(u)}
	if !c.headerLine.valid || !k.projection.reusable() || c.headerLine.key != k {
		c.headerLine = cachedRegionLine{key: k, value: header(s, u, l), valid: true}
	}
	return c.headerLine.value
}

func (c *bodyCache) statistics(s Snapshot, u UIState, l layout) line {
	k := regionLineKey{projection: renderProjection(s, u), width: l.width, theme: u.Theme}
	if !c.statisticsLine.valid || !k.projection.reusable() || c.statisticsLine.key != k {
		c.statisticsLine = cachedRegionLine{key: k, value: statisticsBar(s, u, l), valid: true}
	}
	return c.statisticsLine.value
}

func (c *bodyCache) attentionRows(s Snapshot, u UIState, l layout) []line {
	k := regionLineKey{projection: renderProjection(s, u), width: l.width, theme: u.Theme}
	approval := u.Picker != nil && (u.Picker.Kind == "permissions" || u.Picker.Kind == "permission-once")
	if !c.attention.valid || !k.projection.reusable() || c.attention.key != k || c.attention.approval != approval {
		c.attention = cachedAttention{key: k, approval: approval, rows: attentionBand(s, u, l), valid: true}
	}
	return c.attention.rows
}

type cachedCardRows struct {
	card      workCard
	width     int
	theme     Theme
	alternate bool
	rows      []line
}

func cardRows(c workCard, width int, u UIState, alternate bool) []line {
	if c.main && (strings.HasPrefix(c.now, "待機中（子 ") || strings.HasPrefix(c.now, "会話を圧縮中")) {
		c.now = u.Theme.spinner(u.Now.UnixMilli()) + " " + c.now
	}
	if u.cache == nil {
		return workCardRows(c, width, u.Theme, alternate)
	}
	cache := u.cache
	if cache.cardRows == nil {
		cache.cardRows = make(map[session.ID]cachedCardRows)
	}
	value, ok := cache.cardRows[c.id]
	if !ok || value.card != c || value.width != width || value.theme != u.Theme || value.alternate != alternate {
		value = cachedCardRows{card: c, width: width, theme: u.Theme, alternate: alternate, rows: workCardRows(c, width, u.Theme, alternate)}
		cache.cardRows[c.id] = value
	}
	return value.rows
}

type projectionKey struct {
	id              session.ID
	generation      string
	version, viewer uint64
}

func renderProjection(s Snapshot, u UIState) projectionKey {
	return projectionKey{s.ID, s.Generation, s.Version, u.Viewer.Version}
}

func (k projectionKey) reusable() bool { return k.version != 0 && k.viewer != 0 }

// This index is built once per data update and shared by all receipts/cards.
// In particular, a tool chip must not sort the entire child list on its own.
type renderIndex struct {
	children   []viewer.Row
	marks      map[session.ID]string
	operations map[operation.ID]operation.Operation
	reasons    map[operation.ID]string
}

func (c *bodyCache) prepare(s Snapshot, u UIState) *renderIndex {
	k := renderProjection(s, u)
	if c.index != nil && k.reusable() && c.projection == k {
		return c.index
	}
	u.index = nil
	rows := children(u, s)
	index := &renderIndex{children: rows, marks: childMarks(rows), operations: make(map[operation.ID]operation.Operation, len(s.Operations)), reasons: make(map[operation.ID]string)}
	for _, op := range s.Operations {
		index.operations[op.ID] = op
	}
	c.projection, c.index = k, index
	return index
}

type conversationCache struct {
	key        projectionKey
	width      int
	theme      Theme
	transcript *transcriptPage
	loading    bool
	second     int64
	clock      frameClock
	timed      bool
	rows       []displayRow
	valid      bool
	patches    []timedChips
}

type timedChips struct {
	start, end int
	entry      Entry
}

type chipKey struct {
	body     bodyKey
	sequence uint64
}
type resolvedReceipt struct {
	receipt Receipt
	elapsed viewer.Duration
}
type cachedChips struct {
	resolved  []resolvedReceipt
	width     int
	theme     Theme
	viewer    uint64
	connected bool
	rows      []bodyLine
	seen      uint64
}

func (c *bodyCache) chipRows(e Entry, s Snapshot, u UIState, l layout) ([]bodyLine, bool) {
	k := chipKey{body: bodyKey{e.Role, e.Text, e.PeerID, e.PeerKind, e.Code, e.Clipped, e.UnsafeLinks}, sequence: uint64(e.Sequence)}
	resolved := make([]resolvedReceipt, len(e.Calls))
	timed, spawn := false, false
	for i, raw := range e.Calls {
		r, d := currentReceipt(raw, s, u)
		resolved[i] = resolvedReceipt{r, d}
		timed = timed || s.Connected && !terminalStatus(r.Status)
		spawn = spawn || r.Name == "SubagentStart"
	}
	version := uint64(0)
	if spawn {
		version = u.Viewer.Version
	}
	if c.chips == nil {
		c.chips = make(map[chipKey]cachedChips)
	}
	value, ok := c.chips[k]
	if !ok || spawn && u.Viewer.Version == 0 || value.width != l.text || value.theme != u.Theme || value.viewer != version || value.connected != s.Connected || !reflect.DeepEqual(value.resolved, resolved) {
		value = cachedChips{resolved: resolved, width: l.text, theme: u.Theme, viewer: version, connected: s.Connected, rows: toolChips(e, s, u, l)}
	}
	value.seen = c.frame
	c.chips[k] = value
	return value.rows, timed
}

func (c *bodyCache) conversationRows(s Snapshot, u UIState, l layout) []displayRow {
	k, second := renderProjection(s, u), u.Now.Unix()
	v := &c.conversation
	if k.reusable() && v.valid && v.key == k && v.width == l.text && v.theme == u.Theme && v.transcript == u.Transcript && v.loading == u.HistoryLoading && (!backgroundActive(s, u) || v.clock == renderClock(u)) {
		if v.timed && v.second != second {
			var rows []displayRow
			end := 0
			for i, patch := range v.patches {
				rows = append(rows, v.rows[end:patch.start]...)
				start := len(rows)
				chips, _ := c.chipRows(patch.entry, s, u, l)
				for _, chip := range chips {
					rows = append(rows, displayRow{content: chip.text})
				}
				end = patch.end
				v.patches[i].start, v.patches[i].end = start, len(rows)
			}
			v.rows = append(rows, v.rows[end:]...)
			v.second = second
		}
		return v.rows
	}
	c.begin(l.text, u.Theme.ASCII)
	defer c.end()
	v.patches = nil
	u.collect = v
	rows := conversation(s, u, l)
	*v = conversationCache{key: k, width: l.text, theme: u.Theme, transcript: u.Transcript, loading: u.HistoryLoading, second: second, clock: renderClock(u), timed: len(v.patches) > 0, rows: rows, valid: true, patches: v.patches}
	return rows
}

type frameCache struct {
	key                             projectionKey
	ui                              UIState
	width, height                   int
	clock                           frameClock
	frame                           Frame
	inputStart, inputRows, overflow int
	valid                           bool
}

// Only the composer changes during ordinary typing. Other editor modes use
// the normal layout, whose content regions are cached independently.
func composerCacheUI(u UIState) (UIState, bool) {
	if u.Picker != nil || u.Sheet != nil || u.Transcript != nil || len(u.MenuOptions) != 0 || strings.HasPrefix(u.Input, "/") {
		return UIState{}, false
	}
	u.Input, u.Cursor, u.Now = "", 0, time.Time{}
	u.Viewer, u.Parent, u.cache, u.index, u.collect = viewer.PanelSnapshot{}, nil, nil, nil, nil
	return u, true
}

type frameClock struct{ second, tick int64 }

func renderClock(u UIState) frameClock {
	clock := frameClock{second: u.Now.Unix()}
	if !u.Theme.Still {
		clock.tick = u.Now.UnixMilli() / 120
	}
	return clock
}

func (c *bodyCache) inputFrame(s Snapshot, u UIState, width, height int) (Frame, bool) {
	v := &c.rendered
	key := renderProjection(s, u)
	ui, ok := composerCacheUI(u)
	if !ok || !key.reusable() || !v.valid || key != v.key || width != v.width || height != v.height || renderClock(u) != v.clock || !reflect.DeepEqual(ui, v.ui) {
		return Frame{}, false
	}
	l := makeLayout(width, height)
	if height < 16 {
		return Frame{}, false
	}
	input, cy, cx, overflow, _ := composer(s, u, l)
	if len(input) != v.inputRows || overflow != v.overflow {
		return Frame{}, false
	}
	f := v.frame
	f.Lines = append([]string(nil), f.Lines...)
	for i, row := range input {
		f.Lines[v.inputStart+i] = row.clip(l.width).paint(u.Theme)
	}
	f.CursorY, f.CursorX = v.inputStart+cy, max(0, cx)
	return f, true
}

func (c *bodyCache) rememberFrame(s Snapshot, u UIState, width, height int, f Frame, start, inputRows, overflow int) {
	ui, ok := composerCacheUI(u)
	c.rendered = frameCache{key: renderProjection(s, u), ui: ui, width: width, height: height, clock: renderClock(u), frame: f, inputStart: start, inputRows: inputRows, overflow: overflow, valid: ok && height >= 16}
}

func (c *bodyCache) begin(width int, ascii bool) {
	if c.width != width || c.ascii != ascii || c.entries == nil {
		c.entries = map[bodyKey]cachedBody{}
		c.width = width
		c.ascii = ascii
	}
	c.frame++
}
func (c *bodyCache) get(e Entry, width int, t Theme) []bodyLine {
	k := bodyKey{e.Role, e.Text, e.PeerID, e.PeerKind, e.Code, e.Clipped, e.UnsafeLinks}
	value, ok := c.entries[k]
	if !ok {
		value.rows = entryBody(e, width, t)
	}
	value.seen = c.frame
	c.entries[k] = value
	return value.rows
}
func (c *bodyCache) end() {
	for key, value := range c.entries {
		if value.seen != c.frame {
			delete(c.entries, key)
		}
	}
	for key, value := range c.chips {
		if value.seen != c.frame {
			delete(c.chips, key)
		}
	}
}

func (c *bodyCache) workCards(s Snapshot, u UIState) ([]workCard, []workBead) {
	k := renderProjection(s, u)
	if c.work == nil || !k.reusable() || c.workKey != k {
		c.work = newWorkCardIndex(s, u.Viewer, u.Now)
		c.workKey, c.workSecond = k, u.Now.Unix()
		c.cards, c.beads = c.work.cards, c.work.beads
	} else if c.workSecond != u.Now.Unix() {
		c.cards, c.beads = c.work.snapshot(u.Now)
		c.workSecond = u.Now.Unix()
	}
	return c.cards, c.beads
}
