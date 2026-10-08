package tui

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
)

const multilineChildCommand = "/children start worker openai-codex gpt-6.1-sol medium -- Reply with exactly: Codex child smoke OK.\nThen call Finish alone with status completed and that summary, with empty\nchangedFiles/tests/blockers."

func TestSubmittedCommandUsesExactRegistryNamesAndPreservesPayload(t *testing.T) {
	options := candidates(idleSnapshot(), UIState{Input: "/"})
	if len(options) != len(commandCatalog) {
		t.Fatal("completion and submit use different registries")
	}
	for _, candidate := range options {
		input := " \t\n\u3000" + candidate.Name + "\n日本語 payload\n👩🏽‍💻 é\n"
		got, ok := submittedCommand(input)
		if !ok || got != candidate.Name+"\n日本語 payload\n👩🏽‍💻 é\n" {
			t.Fatalf("registered candidate lost routing/payload: %s", candidate.Name)
		}
	}
	for _, input := range []string{"", " \n", "/", "/vi", "/viewpoint", "/view/chat", "/something-unknown hello", "Please run /view chat", "```\n/view chat\n```"} {
		got, ok := submittedCommand(input)
		if ok || got != input {
			t.Fatalf("nonregistered input classified as a command: %q", input)
		}
	}
	// Adding a command to the shared registry needs no classifier update.
	original := commandCatalog
	commandCatalog = append(append([]commandCandidate(nil), original...), commandCandidate{Name: "/fixture-future", Usage: "/fixture-future TEXT", Args: requiredArguments})
	defer func() { commandCatalog = original }()
	if got, ok := submittedCommand("/fixture-future\nnew payload"); !ok || got != "/fixture-future\nnew payload" {
		t.Fatal("classifier maintained an independent list")
	}
	for _, candidate := range candidates(idleSnapshot(), UIState{Input: "/fixture-"}) {
		if candidate.Name == "/fixture-future" {
			return
		}
	}
	t.Fatal("new registry command absent from completion")
}

func TestRunRegisteredSubmitUsesDispatcherAndNeverChat(t *testing.T) {
	for _, tc := range []struct {
		name, text, reply string
		paste, handled    bool
	}{
		{"single-line", "/children", "command dispatched", false, true},
		{"multiline", multilineChildCommand, "command dispatched", false, true},
		{"pasted-multiline", multilineChildCommand, "command dispatched", true, true},
		{"leading-whitespace", " \t\n /children\n", "command dispatched", true, true},
		{"child", "/child child-public", "command dispatched", true, true},
		{"CJK", "/children start worker -- 日本語の依頼。\n構造を保持してください。\n👩🏽‍💻 é", "command dispatched", true, true},
		{"large-task", "/children start worker -- " + strings.Repeat("line of task 日本語\n", 1600), "command dispatched", true, true},
		{"syntax-error", "/children start worker --\n", "usage: /children start TEMPLATE -- TASK", true, true},
		{"handler-error", "/children start worker -- task\ncontinued", "child request failed: fixture", true, true},
		{"unhandled-registered", "/children\n", "unknown command /children", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoked := make(chan string, 2)
			r := startCommandUXRun(t, [2]int{80, 24}, func(_ context.Context, command string) (string, bool) {
				invoked <- command
				return tc.reply, tc.handled
			})
			before := r.fake.view.History
			r.keys <- Key{Text: tc.text, Paste: tc.paste}
			// A rendered composer acknowledges that the paste was consumed.
			// Neither pasted newlines nor paste completion submit anything.
			waitFor(t, func() bool {
				return strings.Contains(r.out.text(), "pasted:") || !tc.paste && strings.Contains(r.out.rawText(), "/children")
			})
			if len(invoked) != 0 {
				t.Fatal("input executed before Enter")
			}
			r.fake.mu.Lock()
			if len(r.fake.inputs) != 0 {
				t.Error("input submitted before Enter")
			}
			r.fake.mu.Unlock()
			r.keys <- Key{Name: "enter"}
			waitFor(t, func() bool {
				r.fake.mu.Lock()
				defer r.fake.mu.Unlock()
				return len(invoked) > 0 || len(r.fake.inputs) > 0
			})
			r.fake.mu.Lock()
			submitted := len(r.fake.inputs)
			r.fake.mu.Unlock()
			if submitted != 0 {
				t.Fatalf("registered command submitted %d public inputs instead of invoking its handler", submitted)
			}
			waitFor(t, func() bool { return strings.Contains(r.out.text(), tc.reply) })
			if len(invoked) != 1 || <-invoked != strings.TrimLeft(tc.text, " \t\n") {
				t.Fatal("dispatcher lost command/payload or was invoked more than once")
			}
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			if len(r.fake.inputs) != 0 || len(r.fake.view.Operations) != 0 || !reflect.DeepEqual(before, r.fake.view.History) {
				t.Fatal("registered command fell through to public input/history/Operation")
			}
		})
	}
}

func TestRunPastedLocalCommandsNeverBecomePublicConversation(t *testing.T) {
	for _, tc := range []struct{ input, visible string }{
		{"/view orchestration", "Unreal Host validates"},
		{" \t\n/view orchestration\n", "Unreal Host validates"},
		{"/view split", "Unreal Host validates"},
		{"/view invalid\nargument", "usage: /view"},
		{"/export all", "exported to"},
		{"/model", "catalog unavailable"},
		{"/analyze usage", "Overview"},
		{"/help", "Type / to search commands"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			f := conversationFixture("canonical response fixture")
			before := f.view.History
			keys, out, done := make(chan Key, 16), &screenObserver{}, make(chan error, 1)
			go func() {
				done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}, Size: func() (int, int) { return 120, 24 }, ExportDirectory: filepath.Join(t.TempDir(), "exports")})
			}()
			defer func() {
				keys <- Key{Name: "detach"}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}()
			waitFor(t, func() bool { return strings.Contains(out.text(), "canonical response fixture") })
			keys <- Key{Text: tc.input, Paste: true}
			waitFor(t, func() bool { return strings.Contains(out.text(), "pasted:") })
			if strings.Contains(out.text(), tc.visible) {
				t.Fatal("paste executed a command before Enter")
			}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), tc.visible) })
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.inputs) != 0 || len(f.view.Operations) != 0 || !reflect.DeepEqual(before, f.view.History) {
				t.Fatal("local command changed canonical conversation or Operations")
			}
		})
	}
}

func TestRunUnregisteredSlashAndNormalMultilineRemainChat(t *testing.T) {
	for _, value := range []string{
		"/something-unknown hello\n日本語",
		"  /something-unknown hello\ncontinued",
		"Please run /view orchestration\nthen explain.",
		"```text\n/view orchestration\n```",
		"ordinary multiline\n/view orchestration\n👨‍👩‍👧‍👦",
		"/viewpoint is not /view",
	} {
		t.Run(value, func(t *testing.T) {
			r := startCommandUXRun(t, [2]int{80, 24}, func(context.Context, string) (string, bool) {
				t.Error("non-command reached dispatcher")
				return "", false
			})
			r.keys <- Key{Text: value, Paste: true}
			waitFor(t, func() bool { return strings.Contains(r.out.text(), "pasted:") })
			r.fake.mu.Lock()
			if len(r.fake.inputs) != 0 {
				t.Error("paste submitted a message before Enter")
			}
			r.fake.mu.Unlock()
			r.keys <- Key{Name: "enter"}
			waitFor(t, func() bool { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return len(r.fake.inputs) == 1 })
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			var got string
			input := r.fake.inputs[0]
			if input.Kind != inbox.InputExternal || json.Unmarshal(input.Payload, &got) != nil || got != value {
				t.Fatalf("normal message changed: %q", got)
			}
		})
	}
}
