package tui

import (
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func idleSnapshot() Snapshot {
	return Snapshot{ID: "codex-work-1", Connected: true, Running: true, Status: "connected"}
}

func TestLivePhase4CommandsCompletionAndPasteIsolation(t *testing.T) {
	s, u := idleSnapshot(), childUI(2)
	u.Input = "/ch"
	menu := commandSheet(s, u, makeLayout(80, 24))
	if len(menu) < 2 || !strings.Contains(menu[0].plain(), "›") {
		t.Fatal("menu absent")
	}
	var e Editor
	e.Apply(Key{Text: "/logi"})
	var complete completion
	complete.reset()
	u.Input = e.Text()
	complete.apply(&e, s, &u)
	if e.Text() != "/login " {
		t.Fatalf("single completion: %q", e.Text())
	}
	e.replace("/ch")
	complete.reset()
	u.MenuOptions = nil
	u.Input = e.Text()
	complete.apply(&e, s, &u)
	if e.Text() != "/child" {
		t.Fatal("common prefix: " + e.Text())
	}
	complete.apply(&e, s, &u)
	first := e.Text()
	complete.apply(&e, s, &u)
	if first == e.Text() {
		t.Fatal("Tab didn't cycle")
	}
	e.replace("/child codex")
	complete.reset()
	u.MenuOptions = nil
	u.Input = e.Text()
	complete.apply(&e, s, &u)
	if e.Text() != "/child codex-work-1" {
		t.Fatal("parent ID completion")
	}
	u.Input = "/child "
	options := candidates(s, u)
	if len(options) != 3 || options[0].Description != "(this session)" {
		t.Fatal("parent completion order")
	}
	u.Pasted = true
	if len(commandSheet(s, u, makeLayout(80, 24))) != 0 {
		t.Fatal("pasted menu")
	}
	e.Clear()
	e.Apply(Key{Text: "/ch", Paste: true})
	complete.apply(&e, s, &u)
	if e.Text() != "/ch" {
		t.Fatal("pasted Tab executed")
	}
	u.Pasted = false
	u.Private = &credential.Reference{Provider: "openai", ID: "key"}
	if len(commandSheet(s, u, makeLayout(80, 24))) != 0 {
		t.Fatal("private menu")
	}
	var d Decoder
	keys := d.Feed([]byte("\t\x1b[5~\x1b[6~\x1b[200~\t/stop\n\x1b[5~\x1b[201~"))
	if keys[0].Name != "tab" || keys[1].Name != "pageup" || keys[2].Name != "pagedown" {
		t.Fatal(keys)
	}
	for _, key := range keys[3:] {
		if !key.Paste || key.Name != "" {
			t.Fatal("paste acquired a shortcut")
		}
	}
}

func TestLivePhase4MultilineComposerCursorMaskedAndScroll(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Input = "猫👩🏽‍💻é\nline two\nline three\nline four\nline five\nline six"
	u.Cursor = len(u.Input)
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {20, 6}, {120, 40}} {
		f := RenderFrame(s, u, size[0], size[1])
		bounds(t, f, size[0], size[1])
		if size[1] < 30 && !strings.Contains(frameText(f), "lines") {
			t.Fatalf("overflow hidden: %s", frameText(f))
		}
	}
	u.Cursor = len("猫👩🏽‍💻")
	f := RenderFrame(s, u, 80, 24)
	if f.CursorX != 13 {
		t.Fatalf("grapheme cursor: %d", f.CursorX)
	}
	u.Input = strings.Repeat("SECRET", 30)
	u.Cursor = len(u.Input)
	u.Private = &credential.Reference{Provider: "openai", ID: "private-key"}
	f = RenderFrame(s, u, 80, 24)
	if strings.Contains(frameText(f), "SECRET") || strings.Count(frameText(f), "*") != 64 || !strings.Contains(frameText(f), "private: API key for openai/private-key") {
		t.Fatal("private mask: " + frameText(f))
	}
	u = plainUI()
	s.Entries = []Entry{{Role: "agent", Text: strings.Repeat("a canonical line\n", 60) + "LATEST"}}
	f = RenderFrame(s, u, 80, 24)
	if !strings.Contains(frameText(f), "LATEST") {
		t.Fatal("not following latest")
	}
	u.Scroll = 20
	f = RenderFrame(s, u, 80, 24)
	if strings.Contains(frameText(f), "LATEST") || !strings.Contains(frameText(f), "scrollback") || !strings.Contains(f.Lines[2], "agent") {
		t.Fatal("scroll/reinsert role: " + frameText(f))
	}
	s.Progress = &host.Progress{Mode: "streaming", Text: "draft"}
	f = RenderFrame(s, u, 80, 24)
	if !strings.Contains(f.Lines[20], "generating") {
		t.Fatal("scroll pushed pulse out")
	}
}

var sgrRE = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func TestLivePhase4ThemeModesAndInjection(t *testing.T) {
	s, u := idleSnapshot(), childUI(1)
	u.Viewer.Selected = "child-0000"
	u.Input = "/ch"
	u.Cursor = 3
	s.Progress = &host.Progress{Mode: "streaming", Text: "stream 猫"}
	s.Entries = []Entry{{Role: "agent", Text: "**bold**\n```go\ncode\n```"}}
	for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true, Plain: true}} {
		u.Theme = theme
		f := RenderFrame(s, u, 120, 30)
		text := frameText(f)
		for _, match := range sgrRE.FindAllStringSubmatch(text, -1) {
			for _, code := range strings.Split(match[1], ";") {
				if theme.NoColor && (code == "31" || code == "32" || code == "33" || code == "35" || code == "36") {
					t.Fatal("NO_COLOR emitted color")
				}
				if code != "0" && code != "1" && code != "2" && code != "7" && code != "31" && code != "32" && code != "33" && code != "35" && code != "36" {
					t.Fatalf("unsupported/background SGR: %s", code)
				}
			}
		}
		if theme.Plain && sgrRE.MatchString(text) {
			t.Fatal("plain SGR")
		}
		if theme.ASCII {
			for _, r := range text {
				if r > 127 && r != '猫' {
					t.Fatalf("non-ASCII chrome: %q", r)
				}
			}
		}
	}
	env := map[string]string{"LANG": "ja_JP.UTF-8", "NO_COLOR": "1", "TERM": "xterm"}
	theme := EnvironmentTheme(func(k string) string { return env[k] })
	if !theme.NoColor || theme.ASCII || theme.Plain {
		t.Fatal(theme)
	}
	env["TERM"] = "dumb"
	env["LANG"] = "C"
	theme = EnvironmentTheme(func(k string) string { return env[k] })
	if !theme.Plain || !theme.ASCII {
		t.Fatal(theme)
	}
	raw := "\x1b]52;c;data\a\x1b[31m\r\x00\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c"
	safe := SafeText(raw)
	for _, r := range safe {
		if r < 32 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c {
			t.Fatal("unsafe external text")
		}
	}
	u = plainUI()
	u.Input = SafeText(raw)
	u.Notification = Notification{Text: raw, Kind: "error"}
	s.Entries = []Entry{{Role: "agent", Text: raw}}
	s.Progress = &host.Progress{Mode: "streaming", Text: raw}
	f := RenderFrame(s, u, 80, 24)
	if strings.ContainsAny(frameText(f), "\x1b\a\r\u202e\u2066") {
		t.Fatal("terminal injection")
	}
}

func TestLivePhase4DifferentialStreamingRenderer(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Progress = &host.Progress{Mode: "streaming"}
	var renderer Renderer
	first := renderer.Draw(RenderFrame(s, u, 80, 24))
	if strings.Contains(first, "[2J") || !strings.Contains(first, "[?25h") {
		t.Fatal("clear/hidden cursor")
	}
	for i := 1; i <= 40; i++ {
		s.Progress.Text = strings.Repeat("x", i)
		u.Now = u.Now.Add(30 * time.Millisecond)
		frame := RenderFrame(s, u, 80, 24)
		update := renderer.Draw(frame)
		if strings.Contains(update, "[2J") || strings.Contains(update, "[H") || strings.Contains(update, "unreal agent") || strings.Contains(update, "────────────────") {
			t.Fatal("stream repainted unchanged regions")
		}
		if !strings.Contains(update, fmt.Sprintf("\x1b[%d;%dH", frame.CursorY+1, frame.CursorX+1)) {
			t.Fatal("hardware cursor misplaced")
		}
	}
	f := RenderFrame(s, u, 44, 14)
	bounds(t, f, 44, 14)
	update := renderer.Draw(f)
	for _, match := range regexp.MustCompile(`\x1b\[([0-9]+);[0-9]+H`).FindAllStringSubmatch(update, -1) {
		var row int
		fmt.Sscan(match[1], &row)
		if row > 14 {
			t.Fatal("resize wrote outside terminal")
		}
	}
}

func TestLiveBoundedCacheDoesNotRetainInputOrOldWidths(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.cache = &bodyCache{}
	u.Input = "PRIVATE_INPUT_NEVER_CACHED"
	u.Private = &credential.Reference{Provider: "openai", ID: "key"}
	for i := 0; i < 5; i++ {
		s.Entries = []Entry{{Role: "agent", Text: fmt.Sprintf("canonical %d", i)}}
		f := RenderFrame(s, u, 80, 24)
		bounds(t, f, 80, 24)
		if len(u.cache.entries) != 1 {
			t.Fatal("old entries retained in cache")
		}
	}
	for key := range u.cache.entries {
		if strings.Contains(key.text, "PRIVATE_INPUT") {
			t.Fatal("secret/editor cached")
		}
	}
	u.Private = nil
	u.Input = ""
	u.Cursor = 0
	f := RenderFrame(s, u, 44, 14)
	bounds(t, f, 44, 14)
	if len(u.cache.entries) != 1 || u.cache.width != 41 {
		t.Fatal("resize cache not bounded/rebuilt")
	}
}

func TestLiveHeaderUnknownPartialAndAnimationBudget(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Parent = &viewer.Row{Elapsed: viewer.Duration{Known: true, Value: 42 * time.Second}, Usage: viewer.Usage{Known: true, Partial: true, Input: 2000, Output: 30}}
	f := RenderFrame(s, u, 80, 24)
	if !strings.Contains(f.Lines[0], "42s") || !strings.Contains(f.Lines[0], "~") {
		t.Fatal("known/partial metrics lost")
	}
	s.Connected = false
	s.Status = "disconnected"
	s.OfflineSince = time.Unix(90, 0)
	f = RenderFrame(s, u, 80, 24)
	if strings.Contains(f.Lines[0], "42s") || !strings.Contains(f.Lines[0], "unknown") || !strings.Contains(f.Lines[0], "~") {
		t.Fatal("offline metrics stale/unknown")
	}
	u.Busy = true
	u.Outbox = "sending test"
	for _, status := range []string{"connecting", "disconnected", "gap/disconnect; resyncing"} {
		s.Status = status
		f = RenderFrame(s, u, 80, 24)
		count := 0
		for _, r := range frameText(f) {
			if strings.ContainsRune("⠁⠈⠐⠠⢀⡀⠄⠂", r) {
				count++
			}
		}
		if count > 2 {
			t.Fatal("animation budget exceeded")
		}
	}
}

func TestLiveToolTargetsUseRealSchemasAndSpawnFinish(t *testing.T) {
	cases := []struct{ name, args, want string }{{"glob", `{"path":"src","glob":"**/*.go"}`, "**/*.go"}, {"Bash", `{"command":"go test ./...\nSECRET_LINE"}`, "go test ./..."}, {"ast_edit", `{"targets":[{"path":"main.go"}],"replacement":"SECRET"}`, "main.go"}, {"LSP", `{"action":"references","path":"main.go"}`, "references main.go"}, {"DAP", `{"command":"set_breakpoints","source":"main.go"}`, "set_breakpoints main.go"}, {"DAP", `{"command":"launch","start":{"program":"/tmp/app","arguments":["SECRET"]}}`, "launch /tmp/app"}, {"SkillUse", `{"name":"review"}`, "review"}, {"unknown", `{"token":"SECRET","path":"SECRET"}`, ""}}
	for _, c := range cases {
		r := newReceipt(llm.ToolCall{Name: c.name, Arguments: c.args})
		if r.Target != c.want {
			t.Fatalf("%s target %q != %q", c.name, r.Target, c.want)
		}
	}
	s, u := idleSnapshot(), childUI(1)
	u.Viewer.Rows[1].Finish = &viewer.Finish{Status: "completed"}
	s.Entries = []Entry{{Role: "agent", Calls: []Receipt{{Name: "SubagentStart", Target: "task", Operations: []operation.ID{"op-0"}, Status: operation.StatusReady}}}}
	text := frameText(RenderFrame(s, u, 80, 24))
	if !strings.Contains(text, "! spawn") || !strings.Contains(text, "finished: completed") {
		t.Fatal("spawn inferred success from Finish: " + text)
	}
}

func childUI(n int) UIState {
	u := plainUI()
	u.Viewer.ParentID = "codex-work-1"
	u.Viewer.Selected = u.Viewer.ParentID
	u.Viewer.Rows = []viewer.Row{{ID: u.Viewer.ParentID}}
	for i := 0; i < n; i++ {
		u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: session.ID(fmt.Sprintf("child-%04d", i)), ParentID: u.Viewer.ParentID, ParentOperationID: operation.ID(fmt.Sprintf("op-%d", i)), ParentOperationStatus: operation.StatusReady, Label: fmt.Sprintf("Task %d", i), Depth: 1, Runtime: viewer.RuntimeUnknown})
	}
	return u
}

func TestLivePhase3ChildLanesRailFocusAndResponsiveBounds(t *testing.T) {
	s := idleSnapshot()
	for _, n := range []int{0, 1, 3, 9} {
		for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {120, 24}, {160, 40}} {
			u := childUI(n)
			f := RenderFrame(s, u, size[0], size[1])
			bounds(t, f, size[0], size[1])
			text := frameText(f)
			if n == 0 && strings.Contains(text, "child") {
				t.Fatal("child zero noise")
			}
			if n > 0 && size[0] >= 120 {
				if strings.Count(text, "child agents") != 1 {
					t.Fatal("rail absent")
				}
				for _, id := range []string{"child-0000", "child-0001"} {
					if strings.Count(text, id) > 1 {
						t.Fatal("lane duplicated in dock")
					}
				}
			}
			if n == 1 && size[0] == 80 && !strings.Contains(text, "child ▸ child-0000") {
				t.Fatal("single child lane: " + text)
			}
			if n == 9 && size[0] == 80 && (!strings.Contains(text, "9 running") || !strings.Contains(text, "hidden")) {
				t.Fatal("hidden child counts: " + text)
			}
		}
	}
	u := childUI(3)
	u.Viewer.Selected = "child-0001"
	u.Viewer.Rows[2].Generation = "observed"
	u.Viewer.Rows[2].Activity = "edit render.go"
	u.Viewer.Rows[2].Usage = viewer.Usage{Known: true, Input: 12000, Output: 1000}
	for _, w := range []int{80, 120, 160, 60, 80} {
		f := RenderFrame(s, u, w, 30)
		bounds(t, f, w, 30)
		text := frameText(f)
		if !strings.Contains(text, "›") || !strings.Contains(text, "edit render.go") || !strings.Contains(text, "send to codex-work-1") {
			t.Fatalf("focus lost w%d: %s", w, text)
		}
		if u.Viewer.Selected != "child-0001" {
			t.Fatal("selection changed")
		}
	}
	before := children(u, s)
	u.Viewer.Rows[1].ParentOperationStatus = operation.StatusCompleted
	u.Viewer.Rows[2].Activity = "other activity"
	after := children(u, s)
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Fatal("lane reordered")
		}
	}
}

func TestLivePhase3ChildUnknownFinishObservationAndIDs(t *testing.T) {
	s := idleSnapshot()
	u := childUI(3)
	u.Viewer.Rows[1].Activity = "UNOBSERVED_ACTIVITY"
	u.Viewer.Rows[1].Usage = viewer.Usage{Known: true, Input: 777}
	u.Viewer.Selected = u.Viewer.Rows[1].ID
	if text := frameText(RenderFrame(s, u, 80, 30)); strings.Contains(text, "UNOBSERVED") || strings.Contains(text, "777") {
		t.Fatal("unobserved data exposed")
	}
	u.Viewer.Rows[2].Finish = &viewer.Finish{Status: "success", Summary: "done"}
	u.Viewer.Rows[3].Finish = &viewer.Finish{Status: "completed", Summary: "explicit status"}
	s.Connected = false
	s.Status = "disconnected"
	s.OfflineSince = time.Unix(90, 0)
	f := RenderFrame(s, u, 120, 30)
	text := frameText(f)
	if strings.Contains(text, " running") || !strings.Contains(text, "unknown") || !strings.Contains(text, "finished: success") || !strings.Contains(text, "finished: completed") {
		t.Fatal("offline inferred outcome: " + text)
	}
	ids := laneIDs([]viewer.Row{{ID: "prefix-aaaaaaaaaaaaaaaa-1-suffix"}, {ID: "prefix-aaaaaaaaaaaaaaaa-2-suffix"}}, Theme{})
	if ids["prefix-aaaaaaaaaaaaaaaa-1-suffix"] == ids["prefix-aaaaaaaaaaaaaaaa-2-suffix"] {
		t.Fatal("ambiguous IDs")
	}
	for _, id := range ids {
		if strings.Count(id, "…") > 1 {
			t.Fatal("multiple ellipses")
		}
	}
}

func TestLivePhase3ChildHistoryAndTallMetadata(t *testing.T) {
	s := idleSnapshot()
	u := childUI(1)
	u.Viewer.Selected = "child-0000"
	r := &u.Viewer.Rows[1]
	r.Generation = "g"
	r.ProjectInstructions = &projectinstructions.Metadata{SourceKind: projectinstructions.SourceWorkspaceAgents, SourcePath: "AGENTS.md", ByteLength: 123, Digest: "12345678FULLDIGEST"}
	text := frameText(RenderFrame(s, u, 100, 40))
	if !strings.Contains(text, "project") || !strings.Contains(text, "12345678") || strings.Contains(text, "FULLDIGEST") {
		t.Fatal("Tall project metadata: " + text)
	}
	if strings.Contains(frameText(RenderFrame(s, u, 100, 24)), "12345678") {
		t.Fatal("project shown outside Tall")
	}
	u.Viewer.Transcript = &host.HistoryPage{Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "child canonical response"}}}}}}}, NextAfter: 1, More: true}
	text = frameText(RenderFrame(s, u, 100, 30))
	if !strings.Contains(text, "child canonical response") || !strings.Contains(text, "/child-next") || strings.Contains(text, "project") {
		t.Fatal("history didn't replace focus: " + text)
	}
}

func TestLivePhase2DraftCanonicalReplacementAndEpochs(t *testing.T) {
	m := NewModel("s")
	v := host.View{Generation: "g", Revision: 1, Running: true, Progress: &host.Progress{TurnID: "turn", Epoch: 1, Attempt: 1, Mode: "streaming", Text: "**temporary** 猫"}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	u := plainUI()
	f := RenderFrame(m.Snapshot(), u, 80, 24)
	if !strings.Contains(frameText(f), "⋮ **temporary** 猫") || strings.Count(frameText(f), "generating") != 1 {
		t.Fatal(frameText(f))
	}
	v.Revision = 2
	v.History.Items = []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "**temporary** 猫"}}}}}}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	f = RenderFrame(m.Snapshot(), u, 80, 24)
	if strings.Contains(frameText(f), "⋮") || strings.Count(frameText(f), "temporary") != 1 {
		t.Fatal("draft duplicated canonical: " + frameText(f))
	}
	if !m.progress(host.Event{Generation: "g", Revision: 3, Progress: v.Progress}) || m.Snapshot().Progress != nil {
		t.Fatal("completed epoch resurrected")
	}
	if !m.progress(host.Event{Generation: "g", Revision: 4, Progress: &host.Progress{TurnID: "turn", Epoch: 2, Attempt: 2, Mode: "streaming"}}) {
		t.Fatal("next epoch rejected")
	}
	f = RenderFrame(m.Snapshot(), u, 80, 24)
	if !strings.Contains(frameText(f), "retrying (attempt 2)") {
		t.Fatal(frameText(f))
	}
	m.progress(host.Event{Generation: "g", Revision: 5, Progress: &host.Progress{TurnID: "turn", Epoch: 2, Attempt: 1, Mode: "streaming", Text: "old attempt"}})
	if m.Snapshot().Progress.Attempt != 2 {
		t.Fatal("old attempt accepted")
	}
	m.status("disconnected", false)
	if m.Snapshot().Progress != nil || strings.Contains(frameText(RenderFrame(m.Snapshot(), u, 80, 24)), "retrying") {
		t.Fatal("offline draft/pulse")
	}
}

func TestLivePhase2PulsePrioritiesAndStates(t *testing.T) {
	cases := []struct {
		p    *host.Progress
		want string
	}{{&host.Progress{Attempt: 2}, "retrying (attempt 2)"}, {&host.Progress{Mode: "streaming", Text: "draft"}, "generating"}, {&host.Progress{Mode: "streaming"}, "thinking"}, {&host.Progress{Mode: "completed_response"}, "waiting for completed response"}}
	for _, c := range cases {
		s := idleSnapshot()
		s.Progress = c.p
		f := RenderFrame(s, plainUI(), 80, 24)
		if strings.Count(frameText(f), c.want) != 1 || !strings.Contains(f.Lines[20], c.want) {
			t.Fatalf("pulse misplaced: %s", frameText(f))
		}
	}
	s := idleSnapshot()
	s.Operations = []operation.Operation{{ToolName: "edit", Status: operation.StatusReady}, {ToolName: "SubagentStart", Status: operation.StatusReady}}
	if !strings.Contains(frameText(RenderFrame(s, plainUI(), 80, 24)), "running edit") {
		t.Fatal("tool phase")
	}
	s.Operations = s.Operations[1:]
	if pulse(s, plainUI(), makeLayout(80, 24)) != nil {
		t.Fatal("idle parent/child-only pulse")
	}
	s.WaitingForModel = true
	if pulse(s, plainUI(), makeLayout(80, 24)) == nil {
		t.Fatal("input thinking missing")
	}
	s.Running = false
	f := RenderFrame(s, plainUI(), 80, 24)
	if strings.Contains(frameText(f), "thinking") || !strings.Contains(f.Lines[0], "stopped") || !strings.Contains(frameText(f), "/resume continues this session") {
		t.Fatal(frameText(f))
	}
	s.Failure = strings.Repeat("failure detail ", 30)
	f = RenderFrame(s, plainUI(), 80, 24)
	if !strings.Contains(f.Lines[0], "failed") || len(stateDock(s, plainUI(), makeLayout(80, 24))) > 3 {
		t.Fatal("failed state")
	}
	s.Connected = false
	s.Status = "disconnected"
	s.OfflineSince = time.Unix(99, 0)
	f = RenderFrame(s, plainUI(), 80, 24)
	if !strings.Contains(f.Lines[0], "reconnecting") || strings.Contains(frameText(f), "state is unknown until") {
		t.Fatal("short disconnect")
	}
	s.OfflineSince = time.Unix(95, 0)
	f = RenderFrame(s, plainUI(), 80, 24)
	if !strings.Contains(f.Lines[0], "disconnected") || !strings.Contains(f.Lines[0], "unknown") || !strings.Contains(frameText(f), "reconnecting") {
		t.Fatal("sustained disconnect")
	}
	s.Status = "gap/disconnect; resyncing"
	f = RenderFrame(s, plainUI(), 80, 24)
	if !strings.Contains(frameText(f), "rebuilding the view from canonical history") {
		t.Fatal("resync state")
	}
}

func TestLivePhase2ToolsStayAtStepAndCollapseOnlyCompleted(t *testing.T) {
	m := NewModel("s")
	calls := []llm.ToolCall{{CallID: "r1", Name: "read", Arguments: `{"path":"one.go","unexpected":"SECRET"}`}, {CallID: "r2", Name: "read", Arguments: `{"path":"two.go"}`}, {CallID: "e", Name: "edit", Arguments: `{"path":"three.go","new_text":"SECRET"}`}, {CallID: "spawn", Name: "SubagentStart", Arguments: `{"task":"child task"}`}}
	response := llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "I will inspect these files."}}}}
	for _, c := range calls {
		response.Output = append(response.Output, llm.Item{Type: llm.ItemToolCall, Data: c})
	}
	items := []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: response}}}
	statuses := []operation.Status{operation.StatusCompleted, operation.StatusCompleted, operation.StatusFailed, operation.StatusReady}
	var ops []operation.Operation
	for i, c := range calls {
		op := operation.Operation{ID: operation.ID("hidden-op-" + c.CallID), ToolName: c.Name, Status: statuses[i]}
		ops = append(ops, op)
		status := sessionstore.ToolCallStatus{CallID: c.CallID, Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}
		if i == 2 {
			status.Status.Error = "revision mismatch"
		}
		items = append(items, host.HistoryItem{Sequence: sessionstore.Sequence(i + 2), Kind: sessionstore.ItemToolCallStatus, Data: status})
	}
	if err := m.apply(host.View{Generation: "g", Running: true, Operations: ops, History: host.HistoryPage{Items: items}}); err != nil {
		t.Fatal(err)
	}
	f := RenderFrame(m.Snapshot(), plainUI(), 80, 30)
	text := frameText(f)
	for _, want := range []string{"2 tool calls: read 2", "edit  three.go", "failed", "revision mismatch", "spawn  child task"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "hidden-op") || strings.Contains(text, "SECRET") || strings.Count(text, "spawn  child task") != 1 || strings.Count(text, "edit  three.go") != 1 {
		t.Fatal(text)
	}
	if strings.Index(text, "tool calls") < strings.Index(text, "I will inspect") {
		t.Fatal("receipt before step")
	}
	op := ops[2]
	op.Status = operation.StatusCanceled
	s := m.Snapshot()
	s.Operations[2] = op
	if !strings.Contains(frameText(RenderFrame(s, plainUI(), 80, 30)), "canceled") {
		t.Fatal("canceled hidden")
	}
}
func plainUI() UIState         { return UIState{Theme: Theme{Plain: true}, Now: time.Unix(100, 0)} }
func frameText(f Frame) string { return strings.Join(f.Lines, "\n") }
func bounds(t *testing.T, f Frame, w, h int) {
	t.Helper()
	if len(f.Lines) > h {
		t.Fatalf("height %d > %d", len(f.Lines), h)
	}
	for _, l := range f.Lines {
		if !utf8.ValidString(l) || uniseg.StringWidth(l) > max(0, w-1) {
			t.Fatalf("width %d: %q", w, l)
		}
	}
	if f.CursorX >= max(1, w-1) || f.CursorY >= max(1, h) {
		t.Fatalf("cursor out of bounds: %+v", f)
	}
}

func TestLivePhase1IdleResponsiveAndResize(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	for _, size := range [][2]int{{80, 24}, {60, 20}, {44, 14}, {120, 30}, {160, 40}, {40, 6}, {19, 5}, {1, 1}, {0, 0}} {
		f := RenderFrame(s, u, size[0], size[1])
		bounds(t, f, size[0], size[1])
		if size[0] >= 40 && ((!strings.Contains(f.Lines[0], "connected") && !strings.Contains(f.Lines[0], "⇄")) || !strings.Contains(f.Lines[0], "running")) {
			t.Fatalf("header state lost: %q", f.Lines[0])
		}
	}
	f := RenderFrame(s, u, 80, 24)
	if len(f.Lines) != 24 || f.ConversationHeight < 15 || f.Lines[1] != "" || f.Lines[22] != strings.Repeat("─", 79) || !strings.Contains(f.Lines[21], "^D detach") || !strings.Contains(f.Lines[23], "you ›") {
		t.Fatalf("idle layout: %#v", f)
	}
	for _, word := range []string{"child", "/login", "help", "thinking", "generating"} {
		if strings.Contains(frameText(f), word) {
			t.Fatalf("idle noise: %s", word)
		}
	}
	u.Input = "猫👩🏽‍💻hello"
	u.Cursor = len("猫👩🏽‍💻")
	for _, w := range []int{80, 44, 160, 60} {
		f := RenderFrame(s, u, w, 24)
		bounds(t, f, w, 24)
		if !strings.Contains(frameText(f), u.Input) {
			t.Fatalf("input changed on resize: %s", frameText(f))
		}
		if strings.Contains(frameText(f), "▏") {
			t.Fatal("fake caret")
		}
	}
}

func TestLivePhase1ConversationWordsCJKGraphemesCodeAndMarkdown(t *testing.T) {
	for _, value := range []string{"alpha bravo charlie delta echo", "日本語の文章です。確認します（括弧）と句読点、そして続きを表示。", "猫👨‍👩‍👧‍👦é👩🏽‍💻abcdef"} {
		for _, l := range wrapLine(textLine(value, normal), 10, false, 0) {
			if l.width() > 10 || !utf8.ValidString(l.plain()) {
				t.Fatal(l.plain())
			}
			if strings.ContainsAny(string([]rune(l.plain())[0]), noStart) {
				t.Fatalf("kinsoku: %q", l.plain())
			}
		}
	}
	words := wrapLine(textLine("alpha bravo charlie delta", normal), 12, false, 0)
	if words[0].plain() != "alpha bravo" || words[1].plain() != "charlie" {
		t.Fatalf("word split: %#v", words)
	}
	s, u := idleSnapshot(), plainUI()
	s.Entries = []Entry{{Role: "you", Text: "# raw **bold** `code`"}, {Role: "agent", Text: "# Heading\n**bold** [docs](url)\n```go\n    abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz\n```"}}
	f := RenderFrame(s, u, 44, 30)
	bounds(t, f, 44, 30)
	text := frameText(f)
	for _, want := range []string{"# raw **bold** `code`", "Heading", "bold docs (url)", "```go", "↪"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	body := entryBody(s.Entries[1], 20, Theme{})
	if body[0].text[0].style != strong || body[2].text[0].style != meta {
		t.Fatal("heading/fence styles lost")
	}
	if table := entryBody(Entry{Role: "agent", Text: "| **literal** | [text](url) |"}, 70, Theme{}); table[0].text.plain() != "| **literal** | [text](url) |" {
		t.Fatal("unsupported table transformed")
	}
}

func TestLivePhase1BoundedCanonicalDisplay(t *testing.T) {
	value := strings.Repeat("猫👩🏽‍💻é", messageDisplayBytes/10)
	response := host.HistoryItem{Sequence: 1, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: value}}}}}}
	entry := entriesFor(response)[0]
	if !entry.Clipped || len(entry.Text) > messageDisplayBytes || !utf8.ValidString(entry.Text) || !strings.HasSuffix(entry.Text, "é") && !strings.HasSuffix(entry.Text, "猫") && !strings.HasSuffix(entry.Text, "👩🏽‍💻") {
		t.Fatal("grapheme clip failed")
	}
	s := idleSnapshot()
	s.Entries = []Entry{entry}
	f := RenderFrame(s, plainUI(), 80, 24)
	if !strings.Contains(frameText(f), clippedMessage) {
		t.Fatal("clip marker missing")
	}
	m := NewModel("s")
	var items []host.HistoryItem
	for i := 1; i <= 1100; i++ {
		payload, _ := json.Marshal("entry")
		items = append(items, host.HistoryItem{Sequence: sessionstore.Sequence(i), Kind: sessionstore.ItemInput, Data: inbox.Input{Kind: inbox.InputExternal, Payload: payload}})
	}
	if err := m.apply(host.View{History: host.HistoryPage{Items: items}, Running: true}); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot().Entries) != 1024 || !m.Snapshot().OlderDropped {
		t.Fatal("unbounded display")
	}
	u := plainUI()
	u.Scroll = 10000
	f = RenderFrame(m.Snapshot(), u, 100, 24)
	if !strings.Contains(frameText(f), olderMessage) {
		t.Fatal("old-cache boundary missing")
	}
}
