package tui

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// A small VT screen observer lets event-loop tests assert what remains visible,
// rather than treating old output bytes as the current screen. SGR and cursor
// visibility do not alter cells. CSI H/K implement the terminal's public rules.
type screenObserver struct {
	mu    sync.Mutex
	cells [40][160]string
	x, y  int
	raw   bytes.Buffer
}

func (s *screenObserver) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw.Write(p)
	data := string(p)
	for len(data) > 0 {
		if strings.HasPrefix(data, "\x1b[") {
			end := 2
			for end < len(data) && (data[end] < 0x40 || data[end] > 0x7e) {
				end++
			}
			if end >= len(data) {
				break
			}
			args, op := data[2:end], data[end]
			data = data[end+1:]
			switch op {
			case 'H':
				parts := strings.Split(args, ";")
				row, col := 1, 1
				if len(parts) > 0 && parts[0] != "" {
					row, _ = strconv.Atoi(parts[0])
				}
				if len(parts) > 1 {
					col, _ = strconv.Atoi(parts[1])
				}
				s.y = max(0, min(39, row-1))
				s.x = max(0, min(159, col-1))
			case 'K':
				for x := s.x; x < 160; x++ {
					s.cells[s.y][x] = ""
				}
			}
			continue
		}
		end := strings.IndexByte(data, 27)
		if end < 0 {
			end = len(data)
		}
		g := uniseg.NewGraphemes(data[:end])
		for g.Next() {
			if s.x+g.Width() <= 160 {
				s.cells[s.y][s.x] = g.Str()
				for n := 1; n < g.Width(); n++ {
					s.cells[s.y][s.x+n] = ""
				}
				s.x += g.Width()
			}
		}
		data = data[end:]
	}
	return len(p), nil
}
func (s *screenObserver) cursorX() int { s.mu.Lock(); defer s.mu.Unlock(); return s.x }
func (s *screenObserver) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []string
	for _, row := range s.cells {
		var b strings.Builder
		for _, c := range row {
			if c == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c)
			}
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(lines, "\n")
}
func (s *screenObserver) rawText() string { s.mu.Lock(); defer s.mu.Unlock(); return s.raw.String() }

func TestLiveRunHelpCompletionScrollAndPastedUnknownCommand(t *testing.T) {
	f := newFake()
	f.view.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: strings.Repeat("canonical line\n", 60) + "LATEST"}}}}}}}
	keys := make(chan Key, 64)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "LATEST") })
	keys <- Key{Name: "pageup"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "scrollback") && !strings.Contains(out.text(), "LATEST")
	})
	keys <- Key{Name: "pagedown"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "LATEST") && !strings.Contains(out.text(), "scrollback")
	})
	keys <- Key{Text: "/help"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "help") && strings.Contains(out.text(), "Type / to search")
	})
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "Type / to search") })
	f.mu.Lock()
	if len(f.inputs) != 0 {
		t.Fatal("closing help submitted input")
	}
	f.mu.Unlock()
	keys <- Key{Text: "/logi"}
	keys <- Key{Name: "tab"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "cmd › /login") && out.cursorX() == 16 })
	// Clear the completed command with existing editing keys, then feed fragmented paste.
	for i := 0; i < len("/login "); i++ {
		keys <- Key{Name: "backspace"}
	}
	var decoder Decoder
	for _, b := range []byte("\x1b[200~/something-unknown\n猫\x1b[201~") {
		for _, key := range decoder.Feed([]byte{b}) {
			keys <- key
		}
	}
	waitFor(t, func() bool { return strings.Contains(out.text(), "pasted: Enter submits") })
	if strings.Contains(out.text(), "cmd ›") {
		t.Fatal("pasted slash acquired command mode")
	}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.inputs) == 1 })
	f.mu.Lock()
	input := f.inputs[0]
	f.mu.Unlock()
	var value string
	if input.Kind != inbox.InputExternal || json.Unmarshal(input.Payload, &value) != nil || value != "/something-unknown\n猫" {
		t.Fatalf("unknown pasted command changed: %+v", input)
	}
}

type waitingClient struct {
	*fakeClient
	entered chan struct{}
}

func (c *waitingClient) Submit(ctx context.Context, _ session.ID, _ string, input inbox.Input) (host.Receipt, error) {
	c.mu.Lock()
	c.inputs = append(c.inputs, input)
	c.mu.Unlock()
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return host.Receipt{}, ctx.Err()
}
func TestLiveRunPendingCancelWaitingAndPersistentRetry(t *testing.T) {
	f := &waitingClient{fakeClient: newFake(), entered: make(chan struct{}, 2)}
	keys := make(chan Key, 16)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "connected") })
	keys <- Key{Text: "outbox message"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "sending  outbox message") && strings.Contains(out.text(), "^C cancel waiting")
	})
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "request pending; ^C cancels waiting") })
	keys <- Key{Name: "cancel"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "stopped waiting") && strings.Contains(out.text(), "^C stop session")
	})
	f.mu.Lock()
	if len(f.inputs) != 1 || f.inputs[0].Kind != inbox.InputExternal {
		t.Fatal("cancel waiting stopped session")
	}
	first := f.inputs[0].ID
	f.mu.Unlock()
	keys <- Key{Text: "/retry"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.inputs) == 2 })
	f.mu.Lock()
	if f.inputs[1].ID != first {
		t.Fatal("canceled request retry changed ID")
	}
	f.mu.Unlock()
}

type echoingStorage struct{ *fakeClient }

func (c echoingStorage) Login(_ context.Context, _ credential.Reference, secret credential.Secret) (credential.Metadata, error) {
	return credential.Metadata{}, fmt.Errorf("storage echo: %s", secret.Reveal())
}
func TestLiveRunPrivateErrorAndCredentialSheetsNeverLeakValues(t *testing.T) {
	f := echoingStorage{newFake()}
	keys := make(chan Key, 32)
	out := &screenObserver{}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	go func() { done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme}) }()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "connected") })
	keys <- Key{Text: "/login openai private"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "private: API key for openai/private") })
	keys <- Key{Text: "NEVER_DISPLAY_CREDENTIAL_VALUE"}
	keys <- Key{Name: "tab"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "credential storage failed") })
	if strings.Contains(out.rawText(), "NEVER_DISPLAY_CREDENTIAL_VALUE") {
		t.Fatal("credential leaked into output/error")
	}
	keys <- Key{Text: "/methods"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "openai/api_key") && strings.Contains(out.text(), "auth")
	})
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "openai/api_key") })
	keys <- Key{Text: "/credentials"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "no stored credentials") })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("auth sheet or secret sent to session")
	}
}

func TestLiveWarningsAndProviderHintsStayOutsideHistory(t *testing.T) {
	s, u := idleSnapshot(), childUI(5)
	u.Sheet = helpSheet(u.Theme, false)
	u.Input = strings.Repeat("line\n", 8)
	u.Cursor = len(u.Input)
	u.Notification = Notification{Kind: "error", Text: strings.Repeat("not confirmed: details ", 20), Hint: "/retry resends it with the same input ID"}
	s.Entries = []Entry{{Role: "error", Text: "credential file rejected", Code: "external_reauth_required"}, {Role: "error", Text: "writer is already owned", Code: "writer_owned"}}
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {120, 30}} {
		f := RenderFrame(s, u, size[0], size[1])
		bounds(t, f, size[0], size[1])
		text := frameText(f)
		if !strings.Contains(text, "/retry resends") {
			t.Fatal("sheet hid retry: " + text)
		}
	}
	u.Sheet = nil
	u.Notification = Notification{}
	u.Input = ""
	u.Cursor = 0
	u.Viewer = viewer.PanelSnapshot{}
	text := frameText(RenderFrame(s, u, 100, 30))
	if !strings.Contains(text, "codex login") || !strings.Contains(text, "Another client owns the writer") || !strings.Contains(text, "error") {
		t.Fatal("provider hints missing: " + text)
	}
	if hint := failureHint("unknown_code"); hint != "" {
		t.Fatal("invented provider recovery hint")
	}
}
