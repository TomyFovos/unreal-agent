package tui

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
)

func selectedMenuRow(lines []string) string {
	for _, row := range lines {
		row = strings.TrimSpace(row)
		if strings.HasPrefix(row, "› /") || strings.HasPrefix(row, "* /") {
			return row
		}
	}
	return ""
}

func TestCommandMenuArrowsReachEveryCandidateAndScrollBothWays(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {120, 24}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, u := idleSnapshot(), childUI(3)
			menu := menuCatalog()
			var e Editor
			e.replace("/")
			var complete completion
			complete.resetMenu(&u)
			u.Input = e.Text()
			if len(menuOptions(s, u)) != len(menu) {
				t.Fatal("slash did not open the complete menu")
			}
			check := func(i int) Frame {
				t.Helper()
				candidate, ok := selectedCommand(s, u)
				if !ok || candidate.Name != menu[i].Name {
					t.Fatalf("selection %d: %+v", i, candidate)
				}
				f := RenderFrame(s, u, size[0], size[1])
				bounds(t, f, size[0], size[1])
				if !strings.Contains(selectedMenuRow(f.Lines), candidate.Usage) {
					t.Fatalf("selected command hidden at %d: %s", i, frameText(f))
				}
				if f.ConversationHeight < 3 || e.Text() != "/" || e.cursor != 1 {
					t.Fatal("menu navigation altered input or consumed the conversation")
				}
				if size[1] >= 16 && (!strings.Contains(f.Lines[size[1]-4], "^C stop") || !strings.Contains(f.Lines[size[1]-4], "^D detach")) {
					t.Fatal("command navigation hints hid stop/detach")
				}
				u.MenuOffset = f.MenuOffset
				return f
			}
			if f := check(0); !strings.Contains(frameText(f), "↓ more") {
				t.Fatal("missing below-viewport indication")
			}
			for i := 1; i < len(menu); i++ {
				if !complete.move(&e, s, &u, 1) {
					t.Fatal("menu rejected ArrowDown")
				}
				check(i)
			}
			if u.MenuOffset == 0 || !strings.Contains(frameText(check(len(menu)-1)), "↑ more") {
				t.Fatal("menu did not scroll down")
			}
			complete.move(&e, s, &u, 1)
			check(len(menu) - 1)
			for i := len(menu) - 2; i >= 0; i-- {
				complete.move(&e, s, &u, -1)
				check(i)
			}
			complete.move(&e, s, &u, -1)
			check(0)
			if u.MenuOffset != 0 {
				t.Fatal("menu did not scroll back to the top")
			}
		})
	}
}

func TestCommandMenuSelectionSurvivesSmallBudgetsAndResize(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	menu := menuCatalog()
	u.Input = "/"
	for budget := 1; budget <= 8; budget++ {
		u.MenuOffset = 0
		for _, direction := range []int{1, -1} {
			for n := 0; n < len(menu); n++ {
				i := n
				if direction < 0 {
					i = len(menu) - 1 - n
				}
				u.MenuSelection = i
				rows, offset := commandSheetWithin(s, u, makeLayout(80, 24), budget)
				var values []string
				for _, row := range rows {
					values = append(values, row.plain())
				}
				if len(rows) > budget || !strings.Contains(selectedMenuRow(values), menu[i].Usage) {
					t.Fatalf("budget %d selection %d: %v", budget, i, values)
				}
				u.MenuOffset = offset
			}
		}
	}
	u.MenuSelection = len(menu) - 1
	for _, size := range [][2]int{{160, 40}, {44, 14}, {60, 20}, {80, 24}} {
		f := RenderFrame(s, u, size[0], size[1])
		bounds(t, f, size[0], size[1])
		if !strings.Contains(selectedMenuRow(f.Lines), "/help") {
			t.Fatal("resize hid selection")
		}
		u.MenuOffset = f.MenuOffset
	}
	for i, c := range menu {
		if c.Name == "/model" {
			u.MenuSelection = i
		}
	}
	u.Theme = Theme{ASCII: true, Plain: true}
	f := RenderFrame(s, u, 44, 14)
	if !strings.HasPrefix(selectedMenuRow(f.Lines), "* /model") || strings.ContainsAny(frameText(f), "↑↓›─") {
		t.Fatal("menu chrome ignored ASCII mode: " + frameText(f))
	}
}

func TestCommandMenuFilteredNavigationTabAndInputGuards(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	var e Editor
	e.replace("/")
	var complete completion
	complete.resetMenu(&u)
	u.Input = e.Text()
	options := menuOptions(s, u)
	if len(options) < 2 {
		t.Fatal("the menu did not list commands")
	}
	for i := range options {
		if i > 0 {
			complete.move(&e, s, &u, 1)
		}
		candidate, ok := selectedCommand(s, u)
		if !ok || candidate.Name != options[i].Name || !strings.HasPrefix(candidate.Name, "/") {
			t.Fatal("navigation escaped the filtered list")
		}
	}
	complete.move(&e, s, &u, -1)
	selected, _ := selectedCommand(s, u)
	complete.apply(&e, s, &u)
	if e.Text() != commandPrefix(selected) {
		t.Fatalf("Tab did not complete the arrow-selected candidate: %q", e.Text())
	}
	complete.apply(&e, s, &u)
	if e.Text() != commandPrefix(options[len(options)-1]) {
		t.Fatal("repeated Tab cycling regressed")
	}
	for _, mode := range []string{"normal", "pasted", "sheet"} {
		t.Run(mode, func(t *testing.T) {
			u := plainUI()
			var e Editor
			value := "/login"
			switch mode {
			case "normal":
				value = "ordinary input"
			case "sheet":
				u.Sheet = helpSheet(u.Theme, false)
			}
			e.Apply(Key{Text: value, Paste: mode == "pasted"})
			if complete.move(&e, s, &u, 1) || complete.move(&e, s, &u, -1) || len(menuOptions(s, u)) != 0 || e.Text() != value {
				t.Fatal("arrows acquired command behavior outside the menu")
			}
		})
	}
}

func TestCommandMenuArrowDecoderFragmentedAndPasteSafe(t *testing.T) {
	var d Decoder
	var keys []Key
	for _, b := range []byte("\x1b[A\x1b[B\x1b[200~/methods\x1b[A\x1b[B\x1b[201~") {
		keys = append(keys, d.Feed([]byte{b})...)
	}
	// The paste arrives as one key, its arrows as text.
	if len(keys) != 3 || keys[0].Name != "up" || keys[1].Name != "down" || keys[2].Text != "/methods" {
		t.Fatal(keys)
	}
	for _, key := range keys[2:] {
		if key.Name != "" || !key.Paste {
			t.Fatal("pasted arrows became shortcuts")
		}
	}
}

func TestComposerAtBottomWithMultilineAndResponsiveFooter(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	for _, value := range []string{"", "hello", "猫👩🏽‍💻é\nsecond\nthird", strings.Repeat("wrapped 日本語 ", 80)} {
		u.Input, u.Cursor = value, len(value)
		for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {120, 30}} {
			f := RenderFrame(s, u, size[0], size[1])
			bounds(t, f, size[0], size[1])
			if len(f.Lines) != size[1] || f.CursorY != size[1]-2 {
				t.Fatalf("composer/caret not at bottom for %v: %+v", size, f)
			}
			l := makeLayout(size[0], size[1])
			if size[1] < 16 {
				l.text -= len("^C stop") + 2
			}
			input, _, _, _, _ := composer(s, u, l)
			ruleRow := size[1] - len(input) - 2
			if !strings.HasPrefix(f.Lines[ruleRow], "─") {
				t.Fatal("separator did not precede composer")
			}
			if size[1] >= 16 && !strings.Contains(f.Lines[ruleRow-1], "^D detach") {
				t.Fatal("contextual keybar did not precede separator")
			}
		}
	}
}

type commandUXRun struct {
	fake   *fakeClient
	keys   chan Key
	out    *screenObserver
	exited chan struct{}
}

func startCommandUXRun(t *testing.T, size [2]int, command func(context.Context, string) (string, bool)) commandUXRun {
	t.Helper()
	r := commandUXRun{newFake(), make(chan Key, 128), &screenObserver{}, make(chan struct{})}
	done := make(chan error, 1)
	theme := Theme{Plain: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done <- Run(ctx, Config{Client: r.fake, ID: "s", Keys: r.keys, Output: r.out, Theme: &theme, Size: func() (int, int) { return size[0], size[1] }, Command: command})
		close(r.exited)
	}()
	t.Cleanup(func() {
		defer cancel()
		r.keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	waitFor(t, func() bool {
		header := strings.Split(r.out.text(), "\n")[0]
		return strings.Contains(header, "入力待ち") && !strings.Contains(header, "⇄")
	})
	return r
}

func (r commandUXRun) selectCommand(t *testing.T, name string) {
	t.Helper()
	r.keys <- Key{Text: "/"}
	for _, candidate := range menuCatalog() {
		if candidate.Name == name {
			waitFor(t, func() bool {
				return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), candidate.Usage)
			})
			return
		}
		r.keys <- Key{Name: "down"}
	}
	t.Fatal("missing catalog command: " + name)
}

func TestRunCommandMenuEnterExecutesCompleteCandidate(t *testing.T) {
	for _, name := range []string{"/help", "/stop sub", "/stop main", "/stop main+sub", "/stop session", "/resume", "/detach"} {
		t.Run(name, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, func(_ context.Context, line string) (string, bool) {
				return "child command handled", true
			})
			if strings.HasPrefix(name, "/stop") {
				// /stop offers its choices, starting on the main agent.
				r.selectCommand(t, "/stop")
				r.keys <- Key{Name: "enter"}
				waitFor(t, func() bool { return strings.Contains(r.out.text(), "Main agent: interrupt") })
				moves := map[string][]string{"/stop sub": {"up"}, "/stop main+sub": {"down"}, "/stop session": {"down", "down"}}[name]
				for _, move := range moves {
					r.keys <- Key{Name: move}
				}
			} else {
				r.selectCommand(t, name)
			}
			r.keys <- Key{Name: "enter"}
			switch name {
			case "/detach":
				waitFor(t, func() bool {
					select {
					case <-r.exited:
						return true
					default:
						return false
					}
				})
			case "/stop sub", "/stop main", "/stop main+sub", "/stop session":
				want := map[string][]inbox.ControlMode{"/stop sub": {inbox.CancelChildren}, "/stop main": {inbox.InterruptAgent}, "/stop main+sub": {inbox.InterruptAgent, inbox.CancelChildren}, "/stop session": {inbox.StopHard}}[name]
				waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return len(r.fake.inputs) == len(want) })
				r.fake.mu.Lock()
				inputs := slices.Clone(r.fake.inputs)
				r.fake.mu.Unlock()
				for i, input := range inputs {
					var control inbox.ControlMessage
					if input.Kind != inbox.InputControl || json.Unmarshal(input.Payload, &control) != nil || control.Mode != want[i] {
						t.Fatalf("wrong stop command %d: %+v, want %s", i, input, want[i])
					}
				}
			default:
				want := map[string]string{"/help": "Type / to search commands", "/resume": "session resumed"}[name]
				waitFor(t, func() bool { return strings.Contains(r.out.text(), want) })
			}
			if !strings.HasPrefix(name, "/stop") {
				r.fake.mu.Lock()
				defer r.fake.mu.Unlock()
				if len(r.fake.inputs) != 0 {
					t.Fatal("local command became model input")
				}
			}
		})
	}
}

// Commands that agents use, or that /stop and the Split view replace, stay
// out of the menu but still run when typed.
func TestAgentCommandsStayTypableOutsideTheMenu(t *testing.T) {
	commands := make(chan string, 4)
	r := startCommandUXRun(t, [2]int{80, 40}, func(_ context.Context, line string) (string, bool) {
		commands <- line
		return "child command handled", true
	})
	r.keys <- Key{Text: "/"}
	waitFor(t, func() bool { return strings.Contains(r.out.text(), "/model") })
	for _, hidden := range []string{"/logout", "/credentials", "/methods", "/children", "/child-", "/retry", "/child "} {
		if strings.Contains(r.out.text(), hidden) {
			t.Fatalf("the menu lists %s", hidden)
		}
	}
	r.keys <- Key{Name: "backspace"}
	r.keys <- Key{Text: "/child-history"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return len(commands) == 1 })
	if got := <-commands; got != "/child-history" {
		t.Fatalf("typed command dispatched as %q", got)
	}
	waitFor(t, func() bool { return strings.Contains(r.out.text(), "child command handled") })
	r.keys <- Key{Text: "/stop idle"}
	r.keys <- Key{Name: "enter"}
	waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return len(r.fake.inputs) == 1 })
	var control inbox.ControlMessage
	r.fake.mu.Lock()
	input := r.fake.inputs[0]
	r.fake.mu.Unlock()
	if json.Unmarshal(input.Payload, &control) != nil || control.Mode != inbox.StopWhenIdle {
		t.Fatalf("typed /stop idle sent %+v", input)
	}
}

func TestRunCommandMenuFilteredNavigationResetAndSelectedTab(t *testing.T) {
	r := startCommandUXRun(t, [2]int{44, 14}, nil)
	r.keys <- Key{Text: "/"}
	waitFor(t, func() bool { return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/stop") })
	for range 8 {
		r.keys <- Key{Name: "down"}
	}
	waitFor(t, func() bool {
		return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/export")
	})
	r.keys <- Key{Name: "up"}
	waitFor(t, func() bool {
		return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/view")
	})
	r.keys <- Key{Text: "e"}
	waitFor(t, func() bool {
		return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/export") && !strings.Contains(r.out.text(), "/stop")
	})
	r.keys <- Key{Name: "tab"}
	waitFor(t, func() bool {
		return strings.Contains(strings.Split(r.out.text(), "\n")[12], "/export") && r.out.cursorX() == 10+len("/export")
	})
}

func TestRunCommandMenuTabCompletesArrowSelectedCandidate(t *testing.T) {
	for _, name := range []string{"/view", "/copy", "/help"} {
		t.Run(name, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, nil)
			r.selectCommand(t, name)
			r.keys <- Key{Name: "tab"}
			value := name
			waitFor(t, func() bool {
				return strings.Contains(strings.Split(r.out.text(), "\n")[22], "[ cmd ] › "+name) && r.out.cursorX() == 10+len(value)
			})
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			if len(r.fake.inputs) != 0 || r.fake.logins != 0 {
				t.Fatal("Tab executed the selected candidate")
			}
		})
	}
}

func TestRunCommandMenuArrowsPreserveNormalAndPastedInput(t *testing.T) {
	for _, mode := range []string{"normal", "pasted"} {
		t.Run(mode, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, nil)
			value := "/login"
			if mode == "normal" {
				value = "ordinary input"
			}
			r.keys <- Key{Text: value, Paste: mode == "pasted"}
			r.keys <- Key{Name: "down"}
			r.keys <- Key{Name: "up"}
			r.keys <- Key{Name: "tab"}
			offset := 2
			waitFor(t, func() bool { return r.out.cursorX() == offset+len(value) })
			rows := strings.Split(r.out.text(), "\n")
			// The prompt also starts with › now; inspect the region above
			// the composer/statistics rows for an actual selected menu item.
			if selectedMenuRow(rows[:22]) != "" {
				t.Fatal("non-command input opened a menu")
			}
			r.keys <- Key{Name: "enter"}
			if mode == "pasted" {
				waitFor(t, func() bool {
					return strings.Contains(r.out.text(), "ログイン") && strings.Contains(r.out.text(), "codex login")
				})
				r.fake.mu.Lock()
				defer r.fake.mu.Unlock()
				if len(r.fake.inputs) != 0 {
					t.Fatal("registered pasted command became a public input")
				}
			} else {
				waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return len(r.fake.inputs) == 1 })
				r.fake.mu.Lock()
				input := r.fake.inputs[0]
				r.fake.mu.Unlock()
				var text string
				if input.Kind != inbox.InputExternal || json.Unmarshal(input.Payload, &text) != nil || text != value {
					t.Fatal("ordinary input acquired command semantics")
				}
			}
		})
	}
}

// The first view is Split; a command with choices lists them after it is
// chosen, and the choice runs as that full command.
func TestSplitFirstAndChoicesAfterTheCommand(t *testing.T) {
	f := conversationFixture("FIRST VIEW RESPONSE")
	keys, out, done := make(chan Key, 16), &screenObserver{}, make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 160, 30 }})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool {
		header := strings.Split(out.text(), "\n")[0]
		return strings.Contains(out.text(), "FIRST VIEW RESPONSE") && strings.Contains(header, "入力待ち") && !strings.Contains(header, "Split") && strings.Contains(out.text(), "Chat") && strings.Contains(out.text(), "◆ メイン")
	})
	keys <- Key{Text: "/vi"}
	waitFor(t, func() bool { return strings.Contains(selectedMenuRow(strings.Split(out.text(), "\n")), "/view") })
	if strings.Contains(out.text(), "/view split") || strings.Contains(out.text(), "/view chat") {
		t.Fatal("the menu lists a command's choices")
	}
	for range 3 {
		keys <- Key{Name: "backspace"}
	}
	keys <- Key{Text: "/copy"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "Copy to the clipboard") && strings.Contains(out.text(), "すべて") && strings.Contains(out.text(), "Agent  FIRST VIEW RESPONSE")
	})
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "クリップボードに") && strings.Contains(out.text(), "文字コピーしました")
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 {
		t.Fatal("a chosen command became chat input")
	}
}

func TestNarrowTerminalShowsChatForSplit(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.View = ViewSplit
	if f := RenderFrame(s, u, 100, 30); f.View != ViewChat {
		t.Fatal("Split shown below 120 columns", f.View)
	}
	if f := RenderFrame(s, u, 140, 30); f.View != ViewSplit {
		t.Fatal("Split not shown at 140 columns", f.View)
	}
}

// A long paste arrives as one key and shows as a marker in the composer, as
// in Claude Code; submitting sends the whole text.
func TestLongPasteShowsAMarkerAndSubmitsTheText(t *testing.T) {
	var d Decoder
	prompt := strings.Repeat("一行目の指示です。\n", 40)
	keys := d.Feed([]byte("\x1b[200~" + prompt + "\x1b[201~"))
	if len(keys) != 1 || !keys[0].Paste {
		t.Fatalf("paste became %d keys", len(keys))
	}
	var e Editor
	e.Apply(Key{Text: "前置き "})
	e.Apply(keys[0])
	e.Apply(Key{Text: " 以上"})
	if e.Text() != "前置き [Pasted text #1 +40 lines] 以上" {
		t.Fatalf("composer shows %q", e.Text())
	}
	if e.Expanded() != "前置き "+prompt+" 以上" {
		t.Fatal("submitted text lost the paste")
	}
	var short Editor
	short.Apply(Key{Text: "one line", Paste: true})
	if short.Text() != "one line" {
		t.Fatal("a short paste became a marker")
	}
}

// Ctrl+Enter, Shift+Enter, Alt+Enter and Ctrl+J start a new line; Enter
// sends. A trailing backslash before Enter also starts a new line.
func TestNewlineKeys(t *testing.T) {
	var d Decoder
	for _, raw := range []string{"\n", "\x1b\r", "\x1b[13;5u", "\x1b[13;2u", "\x1b[27;5;13~", "\x1b[27;2;13~"} {
		keys := d.Feed([]byte(raw))
		if len(keys) != 1 || keys[0].Name != "newline" || keys[0].Text != "\n" {
			t.Fatalf("%q decoded as %+v", raw, keys)
		}
	}
	if keys := d.Feed([]byte("\r")); len(keys) != 1 || keys[0].Name != "enter" {
		t.Fatalf("Enter decoded as %+v", keys)
	}
	var e Editor
	e.Apply(Key{Text: "一行目"})
	e.Apply(Key{Name: "newline", Text: "\n"})
	e.Apply(Key{Text: "二行目\\"})
	if e.Text() != "一行目\n二行目\\" || !e.continuesLine() {
		t.Fatalf("composer %q", e.Text())
	}
}
