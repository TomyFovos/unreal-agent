package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/tui/filesearch"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func fileFixture(t *testing.T, root, name string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not indexed"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFileMentionAndInsertionUnicodeQuotesMultilineAndSuffix(t *testing.T) {
	for _, test := range []struct {
		value      string
		cursor     int
		path, want string
	}{
		{"@ma", 3, "src/main.go", "src/main.go "},
		{"look @ma suffix", 8, "src/main.go", "look src/main.go suffix"},
		{"look @main.go suffix", 8, "src/new.go", "look src/new.go suffix"},
		{"猫👩🏽‍💻\nread @猫", len("猫👩🏽‍💻\nread @猫"), "猫 👩🏽‍💻é.md", "猫👩🏽‍💻\nread \"猫 👩🏽‍💻é.md\" "},
		{`read @"space fi" later`, len(`read @"space fi`), "space file.txt", `read "space file.txt" later`},
		{`read @"back\\sl`, len(`read @"back\\sl`), `back\slash`, `read "back\\slash" `},
		{"/children start task -- @ma", len("/children start task -- @ma"), "src/main.go", "/children start task -- src/main.go "},
		{"@", 1, "@literal", "\"@literal\" "},
	} {
		var e Editor
		e.Apply(Key{Text: test.value, Paste: true})
		e.cursor = test.cursor
		m, ok := mentionAt(e.Text(), e.cursor)
		if !ok || !insertFileMention(&e, m, test.path) || e.Text() != test.want || !e.Pasted {
			t.Fatalf("%q: %+v %q", test.value, m, e.Text())
		}
		if !utf8Prefix(e.Text(), e.cursor) {
			t.Fatal("UTF-8 cursor split")
		}
	}
	for _, text := range []string{"email@example.com", "done @file next", "first @file\nsecond", "\"@filename\""} {
		if _, ok := mentionAt(text, len(text)); ok {
			t.Fatalf("unintended mention %q", text)
		}
	}
	var e Editor
	e.Apply(Key{Text: strings.Repeat("x", 65530) + " @"})
	m, _ := mentionAt(e.Text(), e.cursor)
	before := e.Text()
	if insertFileMention(&e, m, "too-long-to-fit.txt") || e.Text() != before {
		t.Fatal("composer limit not atomic")
	}
}

func utf8Prefix(value string, cursor int) bool {
	return cursor <= len(value) && SafeText(value[:cursor]) == value[:cursor]
}

func TestFileWorkspaceProjectionUsesCanonicalBindingAcrossAttachAndResume(t *testing.T) {
	root := t.TempDir()
	for _, raw := range []string{`{"Workspace":"` + root + `"}`, `{"Runtime":{"Workspace":"` + root + `"}}`} {
		m := NewModel("s")
		v := host.View{Generation: "first", Running: true, History: host.HistoryPage{Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(raw)}}}}}
		if err := m.apply(v); err != nil || m.Snapshot().Workspace != root {
			t.Fatal(m.Snapshot(), err)
		}
		before := m.Snapshot()
		m.status("disconnected", false)
		v.Generation = "resumed"
		v.History.Items = nil
		if err := m.apply(v); err != nil || m.Snapshot().Workspace != root || m.Snapshot().Generation != "resumed" {
			t.Fatal(m.Snapshot(), err)
		}
		if len(before.Operations) != 0 || len(m.Snapshot().Operations) != 0 {
			t.Fatal("projection created operation")
		}
		v.History.Items = []host.HistoryItem{{Sequence: 2, Kind: sessionstore.ItemFork}}
		if err := m.apply(v); err != nil || m.Snapshot().Workspace != "" {
			t.Fatal("fork retained another workspace", err)
		}
	}
	for _, raw := range []string{`{}`, `{"Workspace":"relative"}`, `{"Workspace":"/tmp/unsafe\n"}`, `{"Workspace":1}`, `{"Runtime":{}}`} {
		if fileWorkspace([]byte(raw)) != "" {
			t.Fatal("unsafe workspace", raw)
		}
	}
}

func TestFileCompletionCancelStaleResultsQueryGenerationAndReconnect(t *testing.T) {
	root := t.TempDir()
	fileFixture(t, root, "a.go")
	fileFixture(t, root, "b.go")
	s := Snapshot{Connected: true, Workspace: root, Generation: "one"}
	var e Editor
	e.replace("@a")
	u := UIState{}
	f := newFileCompletion(t.Context())
	started, canceled := make(chan struct{}), make(chan struct{})
	f.scan = func(ctx context.Context, _ string) (filesearch.Result, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return filesearch.Result{}, ctx.Err()
	}
	f.sync(s, &u, &e)
	<-started
	scanID := f.scanID
	if !f.key(Key{Name: "escape"}, &u, &e, 80, 24) {
		t.Fatal("Escape not consumed")
	}
	<-canceled
	f.sync(s, &u, &e)
	if u.Picker != nil {
		t.Fatal("Escape reopened same mention")
	}
	f.apply(fileEvent{id: scanID, scan: true})
	if u.Picker != nil {
		t.Fatal("late scan reopened picker")
	}
	// New query starts a new scan; the previous generation's result is ignored.
	f.scan = func(ctx context.Context, r string) (filesearch.Result, error) {
		return filesearch.Scan(ctx, r, filesearch.DefaultLimits())
	}
	e.Apply(Key{Text: "b"})
	f.sync(s, &u, &e)
	current := f.scanID
	f.apply(fileEvent{id: scanID, scan: true})
	if !f.scanning {
		t.Fatal("stale scan accepted")
	}
	for f.scanning {
		f.apply(<-f.events)
	}
	f.apply(<-f.events)
	oldSearch := f.searchID
	e.Apply(Key{Name: "backspace"})
	f.sync(s, &u, &e)
	f.apply(fileEvent{id: oldSearch, matches: []filesearch.Match{{Path: "b.go"}}})
	if len(u.Picker.Options) != 0 {
		t.Fatal("old query candidates accepted")
	}
	f.apply(<-f.events)
	if !reflect.DeepEqual(u.Picker.Options, []string{"a.go"}) {
		t.Fatal(u.Picker)
	}
	s.Connected = false
	f.sync(s, &u, &e)
	if u.Picker != nil {
		t.Fatal("offline candidates visible")
	}
	s.Connected = true
	s.Generation = "two"
	f.sync(s, &u, &e)
	f.apply(fileEvent{id: current, scan: true})
	if !f.scanning {
		t.Fatal("old generation accepted")
	}
	s.Workspace = t.TempDir()
	f.sync(s, &u, &e)
	if f.workspace == root {
		t.Fatal("workspace retained across binding switch")
	}
	f.close(&u)
}

func TestFileCompletionPastePrivateModalBusyAndMissingWorkspace(t *testing.T) {
	var e Editor
	e.replace("@")
	f := newFileCompletion(t.Context())
	u := UIState{}
	s := Snapshot{Connected: true, Generation: "g"}
	f.sync(s, &u, &e)
	if u.Picker == nil || u.Picker.Hint != "session workspace unavailable" {
		t.Fatal("used process cwd")
	}
	if f.key(Key{Text: "text", Paste: true}, &u, &e, 80, 24) {
		t.Fatal("paste executed a picker action")
	}
	if !f.key(Key{Name: "enter"}, &u, &e, 80, 24) || e.Text() != "@" {
		t.Fatal("unavailable file picker fell through to submit")
	}
	for _, state := range []UIState{{Busy: true}, {Private: &credential.Reference{}}, {Sheet: &Sheet{}}, {Picker: &Picker{Kind: "view"}}} {
		u = state
		f.sync(s, &u, &e)
		if f.picker != nil || u.Picker != nil && u.Picker.Kind == "file" {
			t.Fatal("file picker stole modal/control input")
		}
	}
}

func TestFilePickerTinyConfirmationAndCandidateReplacementAreFailClosed(t *testing.T) {
	root := t.TempDir()
	fileFixture(t, root, "safe.txt")
	var e Editor
	e.replace("@safe")
	u := UIState{}
	f := newFileCompletion(t.Context())
	f.workspace = root
	f.mention, _ = mentionAt(e.Text(), e.cursor)
	f.picker = &Picker{Kind: "file", Options: []string{"safe.txt"}}
	u.Picker = f.picker
	if !f.key(Key{Name: "enter"}, &u, &e, 16, 8) || e.Text() != "@safe" || f.picker == nil {
		t.Fatal("Tiny blind confirmation")
	}
	if !f.key(Key{Name: "tab"}, &u, &e, 80, 24) || e.Text() != "safe.txt " || u.Picker != nil {
		t.Fatal("resized confirmation", e.Text())
	}
	e.replace("@safe")
	f.mention, _ = mentionAt(e.Text(), e.cursor)
	f.picker = &Picker{Kind: "file", Options: []string{"safe.txt"}}
	u.Picker = f.picker
	if err := os.Remove(filepath.Join(root, "safe.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(root, "safe.txt")); err != nil {
		t.Fatal(err)
	}
	if !f.key(Key{Name: "enter"}, &u, &e, 80, 24) || e.Text() != "@safe" || len(u.Picker.Options) != 0 {
		t.Fatal("stale symlink inserted")
	}
}

func TestFileCompletionWorkerDetachAndSearchDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFileCompletion(ctx)
	var e Editor
	e.replace("@")
	u := UIState{}
	s := Snapshot{Connected: true, Workspace: t.TempDir(), Generation: "g"}
	started, stopped := make(chan struct{}), make(chan struct{})
	f.scan = func(ctx context.Context, _ string) (filesearch.Result, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return filesearch.Result{}, ctx.Err()
	}
	f.sync(s, &u, &e)
	<-started
	f.close(&u)
	cancel()
	<-stopped
	if u.Picker != nil {
		t.Fatal("detach retained candidates")
	}
	// The search receives an actual deadline; its failure cannot submit text or
	// display stale candidates. Use an injected timeout result without sleeping.
	f = newFileCompletion(t.Context())
	f.workspace = s.Workspace
	f.picker = &Picker{Kind: "file"}
	u.Picker = f.picker
	f.search = func(ctx context.Context, _ filesearch.Index, _ string) ([]filesearch.Match, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("missing deadline")
		}
		return nil, context.DeadlineExceeded
	}
	f.startSearch()
	f.apply(<-f.events)
	if len(u.Picker.Options) != 0 || !strings.Contains(u.Picker.Hint, "limit reached") {
		t.Fatal(u.Picker)
	}
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	f.deliver(expired, fileEvent{id: f.searchID, err: context.DeadlineExceeded})
	f.apply(<-f.events)
	if !strings.Contains(u.Picker.Hint, "limit reached") {
		t.Fatal("search timeout vanished")
	}
}

func TestFileDecoderBracketedPasteDoesNotConfirmBeforeEnter(t *testing.T) {
	var decoder Decoder
	var e Editor
	for _, b := range []byte("\x1b[200~inspect\n@src/ma\x1b[201~") {
		for _, key := range decoder.Feed([]byte{b}) {
			if key.Name != "" || !key.Paste {
				t.Fatal("paste executed control key", key)
			}
			e.Apply(key)
		}
	}
	if e.Text() != "inspect\n@src/ma" || !e.Pasted {
		t.Fatal(e.Text())
	}
}

type fileUXRun struct {
	fake     *fakeClient
	keys     chan Key
	out      *screenObserver
	size     atomic.Int64
	commands atomic.Int64
}

func (r *fileUXRun) visible() string {
	rows := strings.Split(r.out.text(), "\n")
	height := int(r.size.Load() & 0xffffffff)
	return strings.Join(rows[:min(height, len(rows))], "\n")
}

func startFileUXRun(t *testing.T, root string) *fileUXRun {
	t.Helper()
	r := &fileUXRun{fake: newFake(), keys: make(chan Key, 64), out: &screenObserver{}}
	r.size.Store(120<<32 | 30)
	raw, _ := json.Marshal(struct{ Workspace string }{root})
	r.fake.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: "configuration", Configuration: raw}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Client: r.fake, ID: "s", Keys: r.keys, Output: r.out, Theme: &Theme{Plain: true}, Size: func() (int, int) { n := r.size.Load(); return int(n >> 32), int(n & 0xffffffff) }, Command: func(context.Context, string) (string, bool) { r.commands.Add(1); return "handled", true }})
	}()
	t.Cleanup(func() {
		defer cancel()
		if t.Failed() {
			t.Log("last terminal frame:\n" + r.visible())
		}
		r.keys <- Key{Name: "detach"}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("file search prevented detach")
		}
	})
	waitFor(t, func() bool { return strings.Contains(r.visible(), "running") })
	return r
}

func TestFileComposerSelectionInsertsOnlyThenSeparateSubmit(t *testing.T) {
	root := t.TempDir()
	fileFixture(t, root, "src/main.go")
	fileFixture(t, root, "docs/猫 👩🏽‍💻é.md")
	for _, confirm := range []string{"enter", "tab"} {
		t.Run(confirm, func(t *testing.T) {
			r := startFileUXRun(t, root)
			r.keys <- Key{Text: "inspect\n@猫", Paste: true}
			waitFor(t, func() bool {
				return strings.Contains(r.visible(), "Enter/Tab insert path") && strings.Contains(r.out.rawText(), "docs/猫 👩🏽‍💻é.md")
			})
			r.keys <- Key{Name: confirm}
			waitFor(t, func() bool {
				return strings.Contains(r.out.rawText(), `"docs/猫 👩🏽‍💻é.md"`) && !strings.Contains(r.visible(), "Enter/Tab insert path")
			})
			r.fake.mu.Lock()
			if len(r.fake.inputs) != 0 || len(r.fake.view.Operations) != 0 || len(r.fake.view.History.Items) != 1 {
				t.Fatal("path insertion created canonical work")
			}
			r.fake.mu.Unlock()
			if r.commands.Load() != 0 {
				t.Fatal("path insertion executed slash handler")
			}
			r.keys <- Key{Name: "enter"}
			waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return len(r.fake.inputs) == 1 })
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			var text string
			if err := json.Unmarshal(r.fake.inputs[0].Payload, &text); err != nil || text != "inspect\n\"docs/猫 👩🏽‍💻é.md\" " || r.fake.inputs[0].Kind != inbox.InputExternal {
				t.Fatal(text, err)
			}
		})
	}
}

func TestFileComposerSlashCommandPasteEscapeAndResizeSplit(t *testing.T) {
	root := t.TempDir()
	fileFixture(t, root, "src/main.go")
	fileFixture(t, root, "src/model.go")
	r := startFileUXRun(t, root)
	r.keys <- Key{Text: "/view split"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(r.visible(), "Orchestration") })
	r.keys <- Key{Text: "/children start worker -- inspect\n@src/ma", Paste: true}
	waitFor(t, func() bool { return strings.Contains(r.visible(), "Enter/Tab insert path") })
	r.keys <- Key{Name: "down"}
	r.keys <- Key{Name: "up"}
	r.keys <- Key{Name: "pageup"}
	r.keys <- Key{Name: "pagedown"}
	r.keys <- Key{Name: "up"}
	r.size.Store(44<<32 | 14)
	waitFor(t, func() bool {
		rows := strings.Split(r.visible(), "\n")
		return strings.Contains(rows[13], "@src/ma") && strings.Contains(r.visible(), "src/main.go")
	})
	r.keys <- Key{Name: "tab"}
	waitFor(t, func() bool {
		return strings.Contains(strings.Split(r.visible(), "\n")[13], "src/main.go") && !strings.Contains(r.visible(), "Enter/Tab insert path")
	})
	if r.commands.Load() != 0 {
		t.Fatal("confirm invoked /children")
	}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return r.commands.Load() == 1 })
	r.fake.mu.Lock()
	if len(r.fake.inputs) != 0 {
		t.Fatal("command fell through to provider")
	}
	r.fake.mu.Unlock()
	r.keys <- Key{Text: "@src"}
	waitFor(t, func() bool { return strings.Contains(r.visible(), "Enter/Tab insert path") })
	r.keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return !strings.Contains(r.visible(), "Enter/Tab insert path") })
	// Escape is stable until the mention/cursor changes, including on resize.
	r.size.Store(160<<32 | 36)
	waitFor(t, func() bool { return strings.HasPrefix(strings.Split(r.visible(), "\n")[34], "─") })
	if strings.Contains(r.visible(), "Enter/Tab insert path") {
		t.Fatal("resize reopened dismissed mention")
	}
}

func TestFilePickerResponsivePlainASCIIAndRightEdge(t *testing.T) {
	for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true, Plain: true}} {
		for _, size := range [][2]int{{1, 4}, {16, 8}, {44, 14}, {60, 20}, {80, 24}, {120, 30}, {180, 40}} {
			s := idleSnapshot()
			u := UIState{Theme: theme, Input: "read @猫", Cursor: len("read @猫"), Picker: &Picker{Kind: "file", Title: "Files", Hint: "Enter/Tab insert path; Esc close", Options: []string{"docs/猫 👩🏽‍💻é.md", strings.Repeat("long/", 80) + "main.go"}}}
			for _, view := range []ViewMode{ViewChat, ViewSplit, ViewOrchestration} {
				u.View = view
				f := RenderFrame(s, u, size[0], size[1])
				plain := f
				plain.Lines = append([]string(nil), f.Lines...)
				for i, line := range plain.Lines {
					plain.Lines[i] = sgrRE.ReplaceAllString(line, "")
				}
				bounds(t, plain, size[0], size[1])
				if f.CursorY != size[1]-1 {
					t.Fatal("file picker moved composer")
				}
				if theme.ASCII && strings.ContainsAny(frameText(f), "↑↓›─") {
					t.Fatal("ASCII picker chrome")
				}
			}
		}
	}
}
