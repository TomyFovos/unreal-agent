package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui/filesearch"
)

// Workspace is projected only from the Session's immutable configuration. A
// missing binding never falls back to the attaching terminal's working directory.
func fileWorkspace(raw []byte) string {
	var c struct {
		Workspace string
		Runtime   *struct{ Workspace string }
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	w := c.Workspace
	if c.Runtime != nil {
		w = c.Runtime.Workspace
	}
	if !filepath.IsAbs(w) || len(w) > 4096 || SafeText(w) != w || strings.ContainsAny(w, "\n\t") {
		return ""
	}
	return filepath.Clean(w)
}

// Byte offsets are aligned to the Editor's UTF-8/grapheme cursor. Mentions are
// limited to the current line and to @ at the start or after whitespace; email
// addresses and embedded prose tokens do not open the picker.
type fileMention struct {
	start, end, cursor int
	query              string
}

func mentionAt(value string, cursor int) (fileMention, bool) {
	if cursor < 0 || cursor > len(value) || !utf8.ValidString(value[:cursor]) {
		return fileMention{}, false
	}
	lineStart := strings.LastIndexByte(value[:cursor], '\n') + 1
	for start := cursor - 1; start >= lineStart; start-- {
		if value[start] != '@' {
			continue
		}
		if start > lineStart {
			before, _ := utf8.DecodeLastRuneInString(value[lineStart:start])
			if !unicode.IsSpace(before) {
				continue
			}
		}
		queryStart, end := start+1, start+1
		quoted := queryStart < len(value) && value[queryStart] == '"'
		if quoted {
			queryStart++
			end = queryStart
			for end < len(value) && value[end] != '"' && value[end] != '\n' {
				if value[end] == '\\' && end+1 < len(value) && value[end+1] != '\n' {
					end++
				}
				end++
			}
			if cursor < queryStart || cursor > end {
				return fileMention{}, false
			}
		} else {
			for end < len(value) {
				r, size := utf8.DecodeRuneInString(value[end:])
				if unicode.IsSpace(r) {
					break
				}
				end += size
			}
			if cursor > end {
				return fileMention{}, false
			}
		}
		query := value[queryStart:cursor]
		if quoted {
			if decoded, err := strconv.Unquote(`"` + query + `"`); err == nil {
				query = decoded
			}
			if end < len(value) && value[end] == '"' {
				end++
			}
		}
		return fileMention{start, end, cursor, query}, true
	}
	return fileMention{}, false
}

func insertFileMention(editor *Editor, mention fileMention, relative string) bool {
	value := editor.Text()
	if !filesearch.SafePath(relative) || mention.start < 0 || mention.end > len(value) || mention.start >= mention.end {
		return false
	}
	text := relative
	if strings.ContainsFunc(text, unicode.IsSpace) || strings.ContainsAny(text, `"\`) || strings.HasPrefix(text, "@") {
		quoted, _ := json.Marshal(text)
		text = string(quoted) // JSON quoting preserves CJK and emoji/ZWJ graphemes.
	}
	suffix := strings.TrimPrefix(value[mention.end:], " ")
	prefix := value[:mention.start] + text + " "
	if len(prefix)+len(suffix) > 65536 {
		return false
	}
	// No Apply(Enter), command dispatch, submission, or file-content read.
	editor.data = []byte(prefix + suffix)
	editor.cursor = len(prefix)
	return true
}

type fileEvent struct {
	id      uint64
	scan    bool
	result  filesearch.Result
	matches []filesearch.Match
	err     error
}

// fileCompletion belongs to a single Run and UI goroutine. Worker results are
// immutable messages; independent IDs invalidate late scan/search responses.
type fileCompletion struct {
	ctx                      context.Context
	events                   chan fileEvent
	scan                     func(context.Context, string) (filesearch.Result, error)
	search                   func(context.Context, filesearch.Index, string) ([]filesearch.Match, error)
	workspace, generation    string
	mention, dismissed       fileMention
	hasDismissed             bool
	picker                   *Picker
	index                    filesearch.Index
	indexResult              filesearch.Result
	scanning                 bool
	scanID, searchID         uint64
	scanCancel, searchCancel context.CancelFunc
}

func newFileCompletion(ctx context.Context) *fileCompletion {
	return &fileCompletion{ctx: ctx, events: make(chan fileEvent, 2),
		scan: func(ctx context.Context, root string) (filesearch.Result, error) {
			return filesearch.Scan(ctx, root, filesearch.DefaultLimits())
		},
		search: func(ctx context.Context, index filesearch.Index, query string) ([]filesearch.Match, error) {
			return index.Search(ctx, query, filesearch.ResultLimit)
		}}
}

func (f *fileCompletion) close(u *UIState) {
	if f.scanCancel != nil {
		f.scanCancel()
		f.scanCancel = nil
	}
	if f.searchCancel != nil {
		f.searchCancel()
		f.searchCancel = nil
	}
	f.scanID++
	f.searchID++
	if u.Picker == f.picker {
		u.Picker = nil
	}
	f.picker, f.index, f.indexResult, f.scanning = nil, filesearch.Index{}, filesearch.Result{}, false
}

func (f *fileCompletion) sync(s Snapshot, u *UIState, e *Editor) {
	if f.workspace != s.Workspace || f.generation != s.Generation {
		f.close(u)
		f.hasDismissed = false
		f.workspace, f.generation = s.Workspace, s.Generation
	}
	mention, active := mentionAt(e.Text(), e.cursor)
	if f.hasDismissed && mention != f.dismissed {
		f.hasDismissed = false
	}
	if !s.Connected || u.Private != nil || u.Sheet != nil || u.Busy || u.Picker != nil && u.Picker != f.picker || !active || f.hasDismissed && mention == f.dismissed {
		f.close(u)
		return
	}
	if f.picker == nil {
		f.mention = mention
		f.picker = &Picker{Kind: "file", Title: "Files", Hint: "scanning workspace filenames"}
		u.Picker = f.picker
		if f.workspace == "" {
			f.picker.Hint = "session workspace unavailable"
			return
		}
		f.scanning = true
		f.scanID++
		id, root := f.scanID, f.workspace
		ctx, cancel := context.WithCancel(f.ctx)
		f.scanCancel = cancel
		go func() {
			defer cancel()
			result, err := f.scan(ctx, root)
			f.deliver(ctx, fileEvent{id: id, scan: true, result: result, err: err})
		}()
		return
	}
	if mention != f.mention {
		changed := mention.query != f.mention.query
		f.mention = mention
		if changed {
			f.startSearch()
		}
	}
}

func (f *fileCompletion) deliver(ctx context.Context, event fileEvent) {
	// A search deadline is a visible bounded-search outcome. Deliver it through
	// the Run lifetime rather than dropping it because its own deadline expired;
	// scope/query IDs still reject superseded outcomes.
	if errors.Is(event.err, context.DeadlineExceeded) {
		ctx = f.ctx
	}
	select {
	case f.events <- event:
	case <-ctx.Done():
	case <-f.ctx.Done():
	}
}

func (f *fileCompletion) startSearch() {
	if f.searchCancel != nil {
		f.searchCancel()
		f.searchCancel = nil
	}
	f.searchID++
	if f.picker == nil {
		return
	}
	f.picker.Options, f.picker.Selection, f.picker.Offset = nil, 0, 0
	if f.scanning || f.workspace == "" {
		return
	}
	f.picker.Hint = "searching filenames"
	id, index, query := f.searchID, f.index, f.mention.query
	ctx, cancel := context.WithTimeout(f.ctx, 250*time.Millisecond)
	f.searchCancel = cancel
	go func() {
		defer cancel()
		matches, err := f.search(ctx, index, query)
		f.deliver(ctx, fileEvent{id: id, matches: matches, err: err})
	}()
}

func (f *fileCompletion) apply(event fileEvent) {
	if f.picker == nil {
		return
	}
	if event.scan {
		if event.id != f.scanID {
			return
		}
		f.scanning = false
		if event.err != nil {
			f.picker.Hint = "workspace filenames unavailable"
			return
		}
		f.index, f.indexResult = event.result.Index, event.result
		f.startSearch()
	} else {
		if event.id != f.searchID {
			return
		}
		if event.err != nil {
			f.picker.Hint = "filename search limit reached; shorten query or Esc close"
			return
		}
		f.picker.Options = nil
		for _, match := range event.matches {
			f.picker.Options = append(f.picker.Options, match.Path)
		}
		f.picker.Hint = "Enter/Tab insert path; Esc close"
		if len(event.matches) == 0 {
			f.picker.Hint = "no matching safe workspace files; Esc close"
		}
		if len(f.mention.query) > filesearch.QueryLimit {
			f.picker.Hint = "query limit: 256 bytes; shorten query"
		}
		if f.indexResult.Limited {
			f.picker.Hint += "; scan limit reached"
		}
		if f.indexResult.Incomplete {
			f.picker.Hint += "; some directories omitted"
		}
	}
}

// Called before all ordinary picker/composer dispatch. Paste only edits the
// composer; confirmation consumes exactly one key and never falls through.
func (f *fileCompletion) key(key Key, u *UIState, e *Editor, width, height int) bool {
	if key.Paste || f.picker == nil || u.Picker != f.picker {
		return false
	}
	p := f.picker
	switch key.Name {
	case "escape":
		f.dismissed, f.hasDismissed = f.mention, true
		f.close(u)
		return true
	case "up", "down", "pageup", "pagedown":
		delta := 1
		if key.Name == "pageup" || key.Name == "pagedown" {
			delta = max(1, height/3-2)
		}
		if key.Name == "up" || key.Name == "pageup" {
			delta = -delta
		}
		p.move(delta)
		return true
	case "enter", "tab":
		if width < 20 || height < 8 {
			p.Hint = "enlarge terminal to select files; Esc close"
			return true
		}
		if len(p.Options) > 0 {
			selected := p.Options[p.Selection]
			if filesearch.Eligible(f.workspace, selected) && insertFileMention(e, f.mention, selected) {
				f.close(u)
			} else {
				p.Options = nil
				p.Hint = "selected path unavailable; Esc close and retry"
			}
		}
		return true
	}
	return false
}
