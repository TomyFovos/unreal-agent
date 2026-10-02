package tui

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestUnicodeEditingFragmentedPasteResizeAndTerminalEscapes(t *testing.T) {
	var d Decoder
	var e Editor
	input := []byte("猫👨‍👩‍👧‍👦é")
	for _, b := range input {
		for _, k := range d.Feed([]byte{b}) {
			e.Apply(k)
		}
	}
	e.Apply(Key{Name: "backspace"})
	if e.Text() != "猫👨‍👩‍👧‍👦" {
		t.Fatalf("combining backspace %q", e.Text())
	}
	e.Apply(Key{Name: "left"})
	e.Apply(Key{Name: "backspace"})
	if e.Text() != "👨‍👩‍👧‍👦" {
		t.Fatal(e.Text())
	}
	e.Apply(Key{Name: "end"})
	e.Apply(Key{Name: "backspace"})
	if e.Text() != "" {
		t.Fatal("ZWJ sequence split")
	}
	for _, b := range []byte("\x1b[200~/stop\n猫\x1b[201~") {
		for _, k := range d.Feed([]byte{b}) {
			if k.Name == "enter" {
				t.Fatal("paste submitted prompt")
			}
			e.Apply(k)
		}
	}
	if !e.Pasted || e.Text() != "/stop\n猫" {
		t.Fatal(e.Text())
	}
	raw := "]52;c;secret猫👩🏽‍💻é" // model data cannot execute terminal clipboard commands
	for width := 1; width < 15; width++ {
		for _, line := range Wrap(raw, width) {
			if strings.ContainsAny(line, "") || !utf8.ValidString(line) || uniseg.StringWidth(line) > width {
				t.Fatalf("unsafe width %d: %q", width, line)
			}
		}
	}
	for _, size := range [][2]int{{1, 4}, {4, 4}, {80, 24}, {2000, 900}} {
		frame := Render(Snapshot{ID: "test", Lines: []string{raw}}, e.Display(false), "status", size[0], size[1], "")
		if len(strings.Split(frame, "\r\n")) > max(4, min(size[1], 200)) {
			t.Fatal("frame exceeds terminal height")
		}
		if !utf8.ValidString(frame) {
			t.Fatal("invalid rendering")
		}
		if !strings.HasPrefix(frame, "\x1b[H\x1b[2J") {
			t.Fatal("missing frame prefix")
		}
	}
}
func TestProjectionNoDuplicatePagesAndLatestOperations(t *testing.T) {
	m := NewModel("s")
	payload, _ := json.Marshal("hello")
	v := host.View{Generation: "one", Revision: 1, Running: true, History: sessionstore.Page{Items: []sessionstore.Item{{Sequence: 1, Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "i", Kind: inbox.InputExternal, Payload: payload}}}, NextAfter: 1}, Operations: []operation.Operation{{ID: "op", Status: operation.StatusAwaiting}}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	v.Generation = "two"
	v.Revision = 0
	v.Operations[0].Status = operation.StatusCompleted
	v.Progress = nil
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if len(s.Lines) != 1 || s.Generation != "two" || s.Operations[0].Status != operation.StatusCompleted {
		t.Fatalf("bad rebuild %+v", s)
	}
	if m.progress(host.Event{Generation: "one", Revision: 2, Progress: &host.Progress{Text: "stale"}}) {
		t.Fatal("stale generation accepted")
	}
	v.History.Items[0].Sequence = 3
	if m.apply(v) == nil {
		t.Fatal("gap ignored")
	}
	m.status("disconnected", false)
	if m.Snapshot().Connected {
		t.Fatal("disconnected liveness")
	}
}

type fakeClient struct {
	mu                           sync.Mutex
	view                         host.View
	events                       chan host.Event
	inputs                       []inbox.Input
	login                        string
	logins, subscribes, inspects int
	failSubmit                   bool
}

func newFake() *fakeClient {
	return &fakeClient{view: host.View{Generation: "g", Running: true}, events: make(chan host.Event, 64)}
}
func (f *fakeClient) Inspect(context.Context, session.ID, sessionstore.Sequence, int) (host.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	return f.view, nil
}
func (f *fakeClient) Subscribe(context.Context, session.ID, sessionstore.Sequence, int, int) (host.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribes++
	return host.Subscription{Initial: f.view, Events: f.events, Cancel: func() {}}, nil
}
func (f *fakeClient) Open(context.Context, host.Mode, session.ID) (host.View, error) {
	return f.view, nil
}
func (f *fakeClient) Submit(_ context.Context, _ session.ID, _ string, input inbox.Input) (host.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, input)
	if f.failSubmit {
		f.failSubmit = false
		return host.Receipt{}, errors.New("disconnected")
	}
	return host.Receipt{ID: input.ID, Sequence: 1}, nil
}
func (f *fakeClient) Methods(context.Context) ([]authflow.Support, error) {
	return authflow.Matrix(), nil
}
func (f *fakeClient) Login(_ context.Context, _ credential.Reference, secret credential.Secret) (credential.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.login = secret.Reveal()
	f.logins++
	return credential.Metadata{}, nil
}
func (f *fakeClient) Logout(context.Context, credential.Reference) error             { return nil }
func (f *fakeClient) ListCredentials(context.Context) ([]credential.Metadata, error) { return nil, nil }

type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *capture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *capture) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestPrivateAuthCancelAndSubmitRetryUseSeparateChannels(t *testing.T) {
	f := newFake()
	f.failSubmit = true
	keys := make(chan Key, 64)
	out := &capture{}
	done := make(chan error, 1)
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out}) }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "connected") })
	command := func(s string) { keys <- Key{Text: s}; keys <- Key{Name: "enter"} }
	command("/login openai key")
	waitFor(t, func() bool { return strings.Contains(out.String(), "API key>") })
	keys <- Key{Text: "canceled-secret"}
	keys <- Key{Name: "cancel"}
	waitFor(t, func() bool { return strings.Contains(out.String(), "login canceled") })
	command("/login openai key")
	keys <- Key{Text: "private-value"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.String(), "API key stored") })
	command("hello")
	waitFor(t, func() bool { return strings.Contains(out.String(), "same input ID") })
	command("/retry")
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.inputs) == 2 })
	waitFor(t, func() bool { return strings.Contains(out.String(), "input committed") })
	keys <- Key{Name: "cancel"}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.inputs) == 3 })
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins != 1 || f.login != "private-value" {
		t.Fatal("private login handling")
	}
	if f.inputs[0].ID != f.inputs[1].ID || f.inputs[0].Kind != inbox.InputExternal || f.inputs[2].Kind != inbox.InputControl {
		t.Fatal("retry/control routing")
	}
	transcript := out.String()
	if strings.Contains(transcript, "private-value") || strings.Contains(transcript, "canceled-secret") {
		t.Fatal("secret displayed")
	}
	for _, i := range f.inputs {
		if strings.Contains(string(i.Payload), "secret") || strings.Contains(string(i.Payload), "private-value") {
			t.Fatal("secret in Inbox")
		}
	}
}
func TestGapReconnectResyncAndProgress(t *testing.T) {
	f := newFake()
	m := NewModel("s")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, f, m, func() {}) }()
	waitFor(t, func() bool { return m.Snapshot().Connected })
	f.events <- host.Event{Generation: "g", Revision: 1, Kind: "progress", Progress: &host.Progress{Text: "partial", TurnID: "t", Epoch: 1}}
	waitFor(t, func() bool { return m.Snapshot().Progress != nil })
	f.mu.Lock()
	f.view.Generation = "new"
	f.view.Revision = 20
	f.view.Operations = []operation.Operation{{ID: "op", Status: operation.StatusCompleted}}
	f.mu.Unlock()
	f.events <- host.Event{Kind: "gap"}
	waitFor(t, func() bool { s := m.Snapshot(); return s.Generation == "new" && s.Connected })
	s := m.Snapshot()
	if s.Progress != nil || len(s.Operations) != 1 || s.Operations[0].Status != operation.StatusCompleted {
		t.Fatal("resync state incorrect")
	}
	cancel()
	<-done
}

type slowOutput struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *slowOutput) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestSlowRendererDoesNotBlockReceiver(t *testing.T) {
	f := newFake()
	out := &slowOutput{entered: make(chan struct{}), release: make(chan struct{})}
	keys := make(chan Key, 1)
	var dimensions atomic.Int32
	dimensions.Store(80)
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Size: func() (int, int) { return int(dimensions.Load()), 24 }})
	}()
	<-out.entered
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.subscribes > 0 })
	f.events <- host.Event{Generation: "g", Revision: 1, Kind: "operation"}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.inspects > 0 })
	dimensions.Store(20)
	close(out.release)
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func FuzzDecoderAndWrap(f *testing.F) {
	f.Add([]byte("猫\x1b[200~paste\x1b[201~"), uint8(20))
	f.Fuzz(func(t *testing.T, input []byte, width uint8) {
		if len(input) > 65536 {
			t.Skip()
		}
		var d Decoder
		var e Editor
		for _, k := range d.Feed(input) {
			e.Apply(k)
		}
		for _, line := range Wrap(e.Display(false), int(width)+1) {
			if !utf8.ValidString(line) || uniseg.StringWidth(line) > int(width)+1 {
				t.Fatal("invalid terminal line")
			}
		}
	})
}

var _ io.Writer = (*capture)(nil)

func TestPanelUpdatesWithoutParentEvents(t *testing.T) {
	f := newFake()
	out := &capture{}
	keys := make(chan Key, 1)
	updates := make(chan struct{}, 1)
	var phase atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Updates: updates, Panel: func(int, int) string {
			if phase.Load() == 0 {
				return "child initial"
			}
			return "child updated"
		}})
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), "child initial") })
	phase.Store(1)
	updates <- struct{}{}
	waitFor(t, func() bool { return strings.Contains(out.String(), "child updated") })
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRenderUnicodeProgress(b *testing.B) {
	snapshot := Snapshot{ID: "bench", Status: "connected", Progress: &host.Progress{Mode: "streaming", Text: strings.Repeat("日本語 progress 👩🏽‍💻 ", 500)}}
	for range 128 {
		snapshot.Lines = append(snapshot.Lines, "agent> Unicode workspace response")
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = Render(snapshot, "> prompt", "connected", 100, 30, "")
	}
}
