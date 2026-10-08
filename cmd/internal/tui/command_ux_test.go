package tui

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
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
			var e Editor
			e.replace("/")
			var complete completion
			complete.resetMenu(&u)
			u.Input = e.Text()
			if len(menuOptions(s, u)) != len(commandCatalog) {
				t.Fatal("slash did not open the complete menu")
			}
			check := func(i int) Frame {
				t.Helper()
				candidate, ok := selectedCommand(s, u)
				if !ok || candidate.Name != commandCatalog[i].Name {
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
				if size[1] >= 16 && (!strings.Contains(f.Lines[size[1]-3], "^C stop") || !strings.Contains(f.Lines[size[1]-3], "^D detach")) {
					t.Fatal("command navigation hints hid stop/detach")
				}
				u.MenuOffset = f.MenuOffset
				return f
			}
			if f := check(0); !strings.Contains(frameText(f), "↓ more") {
				t.Fatal("missing below-viewport indication")
			}
			for i := 1; i < len(commandCatalog); i++ {
				if !complete.move(&e, s, &u, 1) {
					t.Fatal("menu rejected ArrowDown")
				}
				check(i)
			}
			if u.MenuOffset == 0 || !strings.Contains(frameText(check(len(commandCatalog)-1)), "↑ more") {
				t.Fatal("menu did not scroll down")
			}
			complete.move(&e, s, &u, 1)
			check(len(commandCatalog) - 1)
			for i := len(commandCatalog) - 2; i >= 0; i-- {
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
	u.Input = "/"
	for budget := 1; budget <= 8; budget++ {
		u.MenuOffset = 0
		for _, direction := range []int{1, -1} {
			for n := 0; n < len(commandCatalog); n++ {
				i := n
				if direction < 0 {
					i = len(commandCatalog) - 1 - n
				}
				u.MenuSelection = i
				rows, offset := commandSheetWithin(s, u, makeLayout(80, 24), budget)
				var values []string
				for _, row := range rows {
					values = append(values, row.plain())
				}
				if len(rows) > budget || !strings.Contains(selectedMenuRow(values), commandCatalog[i].Usage) {
					t.Fatalf("budget %d selection %d: %v", budget, i, values)
				}
				u.MenuOffset = offset
			}
		}
	}
	u.MenuSelection = len(commandCatalog) - 1
	for _, size := range [][2]int{{160, 40}, {44, 14}, {60, 20}, {80, 24}} {
		f := RenderFrame(s, u, size[0], size[1])
		bounds(t, f, size[0], size[1])
		if !strings.Contains(selectedMenuRow(f.Lines), "/help") {
			t.Fatal("resize hid selection")
		}
		u.MenuOffset = f.MenuOffset
	}
	u.MenuSelection = 5
	u.Theme = Theme{ASCII: true, Plain: true}
	f := RenderFrame(s, u, 44, 14)
	if !strings.HasPrefix(selectedMenuRow(f.Lines), "* /login") || strings.ContainsAny(frameText(f), "↑↓›─") {
		t.Fatal("menu chrome ignored ASCII mode: " + frameText(f))
	}
}

func TestCommandMenuFilteredNavigationTabAndInputGuards(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	var e Editor
	e.replace("/ch")
	var complete completion
	complete.resetMenu(&u)
	u.Input = e.Text()
	options := menuOptions(s, u)
	if len(options) < 2 {
		t.Fatal("child prefix did not filter")
	}
	for i := range options {
		if i > 0 {
			complete.move(&e, s, &u, 1)
		}
		candidate, ok := selectedCommand(s, u)
		if !ok || candidate.Name != options[i].Name || !strings.HasPrefix(candidate.Name, "/ch") {
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
	for _, mode := range []string{"normal", "pasted", "private", "sheet"} {
		t.Run(mode, func(t *testing.T) {
			u := plainUI()
			var e Editor
			value := "/methods"
			switch mode {
			case "normal":
				value = "ordinary input"
			case "private":
				u.Private = &credential.Reference{Provider: "openai", ID: "key"}
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
	if len(keys) != 2+len("/methods") || keys[0].Name != "up" || keys[1].Name != "down" {
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
			if len(f.Lines) != size[1] || f.CursorY != size[1]-1 {
				t.Fatalf("composer/caret not at bottom for %v: %+v", size, f)
			}
			l := makeLayout(size[0], size[1])
			if size[1] < 16 {
				l.text -= len("^C stop") + 2
			}
			input, _, _, _, _ := composer(s, u, l)
			ruleRow := size[1] - len(input) - 1
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
		return strings.Contains(header, "⇄") && strings.Contains(header, "running")
	})
	return r
}

func (r commandUXRun) selectCommand(t *testing.T, name string) {
	t.Helper()
	r.keys <- Key{Text: "/"}
	for _, candidate := range commandCatalog {
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
	for _, name := range []string{"/help", "/retry", "/credentials", "/methods", "/stop", "/stop idle", "/resume", "/detach", "/child-history", "/child-next"} {
		t.Run(name, func(t *testing.T) {
			commands := make(chan string, 1)
			r := startCommandUXRun(t, [2]int{80, 24}, func(_ context.Context, line string) (string, bool) {
				commands <- line
				return "child command handled", true
			})
			r.selectCommand(t, name)
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
			case "/stop", "/stop idle":
				waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return len(r.fake.inputs) == 1 })
				r.fake.mu.Lock()
				input := r.fake.inputs[0]
				r.fake.mu.Unlock()
				var control inbox.ControlMessage
				want := inbox.StopHard
				if name == "/stop idle" {
					want = inbox.StopWhenIdle
				}
				if input.Kind != inbox.InputControl || json.Unmarshal(input.Payload, &control) != nil || control.Mode != want {
					t.Fatalf("wrong stop command: %+v", input)
				}
			case "/child-history", "/child-next":
				waitFor(t, func() bool { return len(commands) == 1 })
				if got := <-commands; got != name {
					t.Fatalf("child dispatch %q != %q", got, name)
				}
			default:
				want := map[string]string{"/help": "Type / to search commands", "/retry": "no uncertain input to retry", "/credentials": "no stored credentials", "/methods": "openai/api_key", "/resume": "session resumed"}[name]
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

func TestRunCommandMenuEnterInsertsRequiredArgumentPrefix(t *testing.T) {
	for _, name := range []string{"/login", "/logout", "/child", "/child-send"} {
		t.Run(name, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, func(_ context.Context, _ string) (string, bool) {
				t.Error("argument command executed before arguments were entered")
				return "unexpected command", true
			})
			r.selectCommand(t, name)
			r.keys <- Key{Name: "enter"}
			waitFor(t, func() bool {
				return strings.Contains(strings.Split(r.out.text(), "\n")[23], "cmd › "+name) && r.out.cursorX() == 9+len(name)+1
			})
			usage := map[string]string{"/login": "/login PROVIDER ID", "/logout": "/logout PROVIDER ID", "/child": "(this session)", "/child-send": "/child-send TEXT"}[name]
			if !strings.Contains(r.out.text(), usage) {
				t.Fatal("argument usage/ID candidates missing")
			}
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			if len(r.fake.inputs) != 0 || r.fake.logins != 0 || strings.Contains(r.out.text(), "private: API key") {
				t.Fatal("prefix entry performed an action")
			}
		})
	}
}

func TestRunCommandMenuFilteredNavigationResetAndSelectedTab(t *testing.T) {
	r := startCommandUXRun(t, [2]int{44, 14}, nil)
	r.keys <- Key{Text: "/ch"}
	waitFor(t, func() bool { return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/children") })
	for range 7 {
		r.keys <- Key{Name: "down"}
	}
	waitFor(t, func() bool {
		return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/child-retry")
	})
	if strings.Contains(r.out.text(), "/login") {
		t.Fatal("filtered menu included login")
	}
	r.keys <- Key{Name: "up"}
	waitFor(t, func() bool {
		return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/child-resume")
	})
	r.keys <- Key{Text: "ild-r"}
	waitFor(t, func() bool {
		return strings.Contains(selectedMenuRow(strings.Split(r.out.text(), "\n")), "/child-resume") && !strings.Contains(r.out.text(), "/child-send")
	})
	r.keys <- Key{Name: "down"}
	r.keys <- Key{Name: "tab"}
	waitFor(t, func() bool {
		return strings.Contains(strings.Split(r.out.text(), "\n")[13], "/child-retry") && r.out.cursorX() == 2+len("/child-retry")
	})
}

func TestRunCommandMenuTabCompletesArrowSelectedCandidate(t *testing.T) {
	for _, name := range []string{"/login", "/child-send", "/help"} {
		t.Run(name, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, nil)
			r.selectCommand(t, name)
			r.keys <- Key{Name: "tab"}
			value := name
			if name != "/help" {
				value += " "
			}
			waitFor(t, func() bool {
				return strings.Contains(strings.Split(r.out.text(), "\n")[23], "cmd › "+name) && r.out.cursorX() == 9+len(value)
			})
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			if len(r.fake.inputs) != 0 || r.fake.logins != 0 {
				t.Fatal("Tab executed the selected candidate")
			}
		})
	}
}

func TestRunCommandMenuArrowsPreserveNormalPastedAndPrivateInput(t *testing.T) {
	for _, mode := range []string{"normal", "pasted", "private"} {
		t.Run(mode, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, nil)
			if mode == "private" {
				r.keys <- Key{Text: "/login openai key"}
				r.keys <- Key{Name: "enter"}
				waitFor(t, func() bool { return strings.Contains(r.out.text(), "private: API key") })
			}
			value := "/methods"
			if mode == "normal" {
				value = "ordinary input"
			}
			r.keys <- Key{Text: value, Paste: mode == "pasted"}
			r.keys <- Key{Name: "down"}
			r.keys <- Key{Name: "up"}
			r.keys <- Key{Name: "tab"}
			waitFor(t, func() bool { return r.out.cursorX() == 9+len(value) })
			if selectedMenuRow(strings.Split(r.out.text(), "\n")) != "" {
				t.Fatal("non-command input opened a menu")
			}
			r.keys <- Key{Name: "enter"}
			if mode == "private" {
				waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return r.fake.logins == 1 })
				r.fake.mu.Lock()
				defer r.fake.mu.Unlock()
				if r.fake.login != value || len(r.fake.inputs) != 0 || strings.Contains(r.out.rawText(), value) {
					t.Fatal("private input altered, disclosed, or sent to the session")
				}
			} else if mode == "pasted" {
				waitFor(t, func() bool {
					return strings.Contains(r.out.text(), "auth") && strings.Contains(r.out.text(), "openai/")
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
