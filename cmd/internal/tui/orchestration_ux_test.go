package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func viewCommand(keys chan<- Key, command string) {
	keys <- Key{Text: command}
	keys <- Key{Name: "enter"}
}

func TestOrchestrationRunPickerDirectCommandsAndComposerEditing(t *testing.T) {
	f := conversationFixture("VIEW CHAT RESPONSE")
	keys := make(chan Key, 32)
	out := &screenObserver{}
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 120, 24 }, Command: func(context.Context, string) (string, bool) {
			t.Error("/view delegated to a Host command")
			return "", false
		}})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "VIEW CHAT RESPONSE") })
	viewCommand(keys, "/view")
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Chat") && strings.Contains(out.text(), "Split") })
	keys <- Key{Name: "down"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Orchestration") })
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Unreal Host validates") && !strings.Contains(out.text(), "VIEW CHAT RESPONSE")
	})
	viewCommand(keys, "/view")
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Orchestration") })
	keys <- Key{Name: "tab"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Split") })
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Unreal Host validates") && strings.Contains(out.text(), "VIEW CHAT RESPONSE")
	})
	viewCommand(keys, "/view chat")
	waitFor(t, func() bool {
		return !strings.Contains(out.text(), "Unreal Host validates") && strings.Contains(out.text(), "VIEW CHAT RESPONSE")
	})
	viewCommand(keys, "/view orchestration")
	waitFor(t, func() bool { return strings.Contains(out.text(), "Unreal Host validates") })
	viewCommand(keys, "/view split")
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "VIEW CHAT RESPONSE") && strings.Contains(out.text(), "Unreal Host validates")
	})
	viewCommand(keys, "/view")
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Split") })
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "› Split") })
	keys <- Key{Text: "ab猫"}
	keys <- Key{Name: "left"}
	keys <- Key{Text: "x"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "you › abx猫") })
	keys <- Key{Name: "right"}
	keys <- Key{Name: "backspace"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "you › abx") && !strings.Contains(out.text(), "abx猫")
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 || len(f.view.Operations) != 0 || f.logins != 0 {
		t.Fatal("viewing created canonical work or performed auth")
	}
}

func TestOrchestrationRunResizePreservesSplitAndPickerAvailability(t *testing.T) {
	f := conversationFixture("RESIZE CHAT")
	var width atomic.Int64
	width.Store(120)
	keys, out, done := make(chan Key, 32), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return int(width.Load()), 24 }})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "RESIZE CHAT") })
	viewCommand(keys, "/view split")
	waitFor(t, func() bool { return strings.Contains(out.text(), "Unreal Host validates") })
	width.Store(80)
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Split returns at 120 columns") && !strings.Contains(out.text(), "Unreal Host validates")
	})
	width.Store(120)
	waitFor(t, func() bool { return strings.Contains(out.text(), "Unreal Host validates") })
	viewCommand(keys, "/view")
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Split") })
	width.Store(60)
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Split requires 120 columns") && strings.Contains(out.text(), "› Chat") && !strings.Contains(out.text(), "› Split")
	})
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "› Chat") })
	viewCommand(keys, "/view split")
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Split requires 120 columns") && !strings.Contains(out.text(), "Unreal Host validates")
	})
	viewCommand(keys, "/view invalid")
	waitFor(t, func() bool { return strings.Contains(out.text(), "usage: /view") })
	width.Store(120)
	waitFor(t, func() bool { return strings.Contains(out.text(), "Unreal Host validates") })
}

func TestOrchestrationRunScrollRegionsAndSheetsPreserveTranscriptPosition(t *testing.T) {
	f := conversationFixture(strings.Repeat("conversation visual line\n", 90) + "CHAT TAIL")
	panel := viewer.PanelSnapshot{ParentID: "s", Selected: "s", Rows: []viewer.Row{{ID: "s", Generation: "g"}}}
	for i := range 40 {
		panel.Rows[0].Operations = append(panel.Rows[0].Operations, viewer.OperationRow{ID: operation.ID(fmt.Sprint(i)), Tool: fmt.Sprintf("scroll-tool-%02d", i), Status: operation.StatusCompleted})
	}
	keys, out, done := make(chan Key, 32), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Viewer: func(time.Time) viewer.PanelSnapshot { return panel }})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "CHAT TAIL") })
	keys <- Key{Name: "pageup"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "scrollback:") && !strings.Contains(out.text(), "CHAT TAIL")
	})
	viewCommand(keys, "/view orchestration")
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Orchestration: line 1") && strings.Contains(out.text(), "scroll-tool-00")
	})
	keys <- Key{Name: "pagedown"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Orchestration: line") && !strings.Contains(out.text(), "Orchestration: line 1;") && !strings.Contains(out.text(), "scroll-tool-00")
	})
	keys <- Key{Name: "pageup"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "scroll-tool-00") })
	viewCommand(keys, "/help")
	waitFor(t, func() bool { return strings.Contains(out.text(), "Type / to search") })
	keys <- Key{Name: "pagedown"}
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool {
		return !strings.Contains(out.text(), "Type / to search") && strings.Contains(out.text(), "Orchestration: line 1")
	})
	viewCommand(keys, "/analyze")
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Overview") })
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Analysis · Overview") })
	keys <- Key{Name: "pagedown"}
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Overview") })
	keys <- Key{Name: "escape"}
	viewCommand(keys, "/view chat")
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "scrollback:") && !strings.Contains(out.text(), "Unreal Host validates")
	})
	keys <- Key{Name: "pagedown"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "CHAT TAIL") && !strings.Contains(out.text(), "scrollback:")
	})
}

func TestOrchestrationRunLiveUpdatesPickerRuntimeSwitchReconnectAndExport(t *testing.T) {
	f := conversationFixture("LIVE ORIGINAL RESPONSE")
	selection := &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "opus", Effort: "high", Revision: 7}
	seq := sessionstore.Sequence(len(f.view.History.Items) + 1)
	initialRuntime := host.HistoryItem{Sequence: seq, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: selection}}
	f.view.History.Items = append(f.view.History.Items, initialRuntime)
	_, u := orchestrationFixture()
	u.Viewer.ParentID, u.Viewer.Selected = "s", "s"
	u.Viewer.Rows[0].ID = "s"
	for i := 1; i < len(u.Viewer.Rows); i++ {
		u.Viewer.Rows[i].ParentID = "s"
	}
	// Only one child makes its status visible even while a picker uses dock rows.
	u.Viewer.Rows = u.Viewer.Rows[:2]
	var mu sync.Mutex
	updates := make(chan struct{}, 1)
	keys, out, done := make(chan Key, 32), &screenObserver{}, make(chan error, 1)
	exports := filepath.Join(t.TempDir(), "exports")
	var observation atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		done <- Run(ctx, Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 160, 40 }, Updates: updates, ExportDirectory: exports,
			ViewChanged: func(v ViewMode) { observation.Store(v != ViewChat) },
			Viewer: func(time.Time) viewer.PanelSnapshot {
				mu.Lock()
				defer mu.Unlock()
				p := u.Viewer
				p.Rows = append([]viewer.Row(nil), p.Rows...)
				return p
			},
			ModelCatalog: func(context.Context) (modelcatalog.Catalog, error) {
				return modelcatalog.Catalog{Available: true, Models: []modelcatalog.Model{{ID: "opus", Name: "Synthetic Opus", Efforts: []llm.ReasoningEffort{"high"}}}}, nil
			},
		})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if observation.Load() {
			t.Error("detach retained topology observation")
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "LIVE ORIGINAL RESPONSE") })
	viewCommand(keys, "/view orchestration")
	waitFor(t, func() bool {
		return observation.Load() && strings.Contains(out.text(), "child-a") && strings.Contains(out.text(), "go test ./...")
	})
	viewCommand(keys, "/model")
	waitFor(t, func() bool { return strings.Contains(out.text(), "Synthetic Opus") })
	mu.Lock()
	u.Viewer.Rows[1].Finish = &viewer.Finish{Status: "completed"}
	u.Viewer.Rows[1].Operations = nil
	mu.Unlock()
	updates <- struct{}{}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "child-a  done") && !strings.Contains(out.text(), "go test ./...")
	})
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "Synthetic Opus") })
	// Parent provider/revision changes are canonical observations. The child
	// keeps its independent Codex selection after this switch and a reconnect.
	f.mu.Lock()
	next := &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "gpt-switch", Effort: "medium", Revision: 8}
	item := host.HistoryItem{Sequence: seq + 1, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostRuntimeApplied, Selection: next}}
	f.view.History.Items = append(f.view.History.Items, item)
	f.view.History.NextAfter = seq + 1
	f.view.Revision++
	revision := f.view.Revision
	f.mu.Unlock()
	f.events <- host.Event{Kind: "item", Generation: "g", Revision: revision, Item: &item}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Codex · gpt-switch · medium") && strings.Contains(out.text(), "r8") && strings.Contains(out.text(), "Codex · gpt-fixture · medium")
	})
	f.events <- host.Event{Kind: "disconnected"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "unknown") })
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "parent s  running") && strings.Contains(out.text(), "r8")
	})
	viewCommand(keys, "/export")
	waitFor(t, func() bool { return strings.Contains(out.text(), "Export response") })
	keys <- Key{Name: "enter"}
	paths := exportFiles(t, exports, 1)
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "exported to") && !strings.Contains(out.text(), "cancel waiting")
	})
	if exportBody(t, paths[0]) != "LIVE ORIGINAL RESPONSE" {
		t.Fatal("view affected canonical conversation export")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 || len(f.view.Operations) != 0 {
		t.Fatal("view/export produced canonical work")
	}
}

func TestOrchestrationViewCommandDiscoveryPastedAndPrivateSafety(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Input = "/vi"
	options := candidates(s, u)
	if len(options) != 4 {
		t.Fatal("view command variants absent", options)
	}
	if !strings.Contains(strings.Join(helpSheet(u.Theme, false).Lines, "\n"), "/view orchestration") {
		t.Fatal("help not updated")
	}
	var editor Editor
	var complete completion
	editor.replace("/vi")
	complete.reset()
	complete.apply(&editor, s, &u)
	if editor.Text() != "/view" {
		t.Fatal("view common-prefix completion", editor.Text())
	}
	complete.move(&editor, s, &u, 2)
	complete.apply(&editor, s, &u)
	if editor.Text() != "/view orchestration" {
		t.Fatal("arrow selection + Tab completion", editor.Text())
	}
	f := conversationFixture("paste safety response")
	keys, out, done := make(chan Key, 32), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "paste safety response") })
	keys <- Key{Text: "/view orchestration", Paste: true}
	waitFor(t, func() bool { return strings.Contains(out.text(), "pasted: Enter submits") })
	if strings.Contains(out.text(), "Unreal Host validates") {
		t.Fatal("paste executed view command before Enter")
	}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Unreal Host validates") })
	f.mu.Lock()
	if len(f.inputs) != 0 || len(f.view.Operations) != 0 {
		t.Error("pasted view command created public input or an Operation")
	}
	f.mu.Unlock()
	viewCommand(keys, "/view chat")
	waitFor(t, func() bool { return strings.Contains(out.text(), "paste safety response") })
	viewCommand(keys, "/login openai api")
	waitFor(t, func() bool { return strings.Contains(out.text(), "private: API key") })
	keys <- Key{Text: "/view orchestration"}
	keys <- Key{Name: "down"}
	keys <- Key{Name: "tab"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "*******************") })
	if strings.Contains(out.rawText(), "key › /view") || strings.Contains(out.text(), "Unreal Host validates") {
		t.Fatal("private input acquired view navigation")
	}
	keys <- Key{Name: "cancel"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "key entry canceled") })
	if f.logins != 0 {
		t.Fatal("private view text stored without submit")
	}
}

func TestOrchestrationViewDoesNotMutateCanonicalHistoryOrCreateOperations(t *testing.T) {
	f := conversationFixture("unchanged canonical response")
	before := append([]host.HistoryItem(nil), f.view.History.Items...)
	keys, out, done := make(chan Key, 16), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}})
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "unchanged canonical response") })
	for _, mode := range []string{"orchestration", "chat", "orchestration"} {
		viewCommand(keys, "/view "+mode)
		waitFor(t, func() bool { return strings.Contains(out.text(), "Unreal Host validates") == (mode == "orchestration") })
	}
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if !reflect.DeepEqual(before, f.view.History.Items) || len(f.view.Operations) != 0 || len(f.inputs) != 0 {
		t.Fatal("viewing changed canonical state")
	}
	f.mu.Unlock()
	// Reattaching creates a fresh Chat preference and reprojects the same history.
	keys, out, done = make(chan Key, 16), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}})
	}()
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "unchanged canonical response") && !strings.Contains(out.text(), "Unreal Host validates")
	})
	keys <- Key{Name: "detach"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
