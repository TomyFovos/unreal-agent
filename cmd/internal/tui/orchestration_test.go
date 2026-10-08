package tui

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func orchestrationFixture() (Snapshot, UIState) {
	s, u := idleSnapshot(), plainUI()
	s.ID, s.TurnID = "main", "turn-7"
	s.Selection = &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "opus", Effort: "high", Revision: 7, Binding: "credential-binding-never-shown"}
	s.Entries = []Entry{{Role: "you", Text: "raw-prompt-never-shown"}, {Role: "agent", Text: "raw-response-never-shown"}}
	s.Progress = &host.Progress{Mode: "streaming", Text: "draft-never-shown"}
	u.Viewer = viewer.PanelSnapshot{ParentID: s.ID, Selected: s.ID, Rows: []viewer.Row{
		{ID: s.ID, Selection: s.Selection, Runtime: viewer.RuntimeRunning, Generation: "g", ObservedModel: "claude-opus-fixture"},
		{ID: "child-a", ParentID: s.ID, Depth: 1, ParentOperationID: "spawn-a", ParentOperationStatus: operation.StatusReady, Generation: "child-a-generation", Label: "inspect parser", Selection: &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "gpt-fixture", Effort: "medium", Revision: 2}, Operations: []viewer.OperationRow{
			{ID: "read-a", Tool: "read", Target: "stream.go", Status: operation.StatusCompleted, Sequence: 3},
			{ID: "test-a", Tool: "Bash", Target: "go test ./...", Status: operation.StatusReady, Sequence: 4},
		}},
		{ID: "child-b", ParentID: s.ID, Depth: 1, ParentOperationID: "spawn-b", ParentOperationStatus: operation.StatusCompleted, Label: "update tests", Finish: &viewer.Finish{Status: "completed", RecordedAt: time.Unix(3, 0), Summary: "private-finish-body-never-shown"}, Selection: &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "sonnet", Effort: "high", Revision: 1}},
		{ID: "child-c", ParentID: s.ID, Depth: 1, ParentOperationID: "spawn-c", ParentOperationStatus: operation.StatusFailed, Label: "check failure", Selection: &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "gpt-other", Effort: "low"}},
	}}
	return s, u
}

func topologyNode(t *testing.T, o OrchestrationSnapshot, id string) OrchestrationNode {
	t.Helper()
	for _, n := range o.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("missing topology node %s", id)
	return OrchestrationNode{}
}

func TestOrchestrationProjectionCrossProviderOwnershipAndIndependentRuntime(t *testing.T) {
	s, u := orchestrationFixture()
	beforeRows, err := json.Marshal(u.Viewer.Rows, json.Deterministic(true), jsonv1.FormatDurationAsNano(true))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.Selection)
	o := BuildOrchestration(s, u.Viewer, u.Now, 128)
	parent, a, b, c := topologyNode(t, o, "agent:main"), topologyNode(t, o, "agent:child-a"), topologyNode(t, o, "agent:child-b"), topologyNode(t, o, "agent:child-c")
	if o.Session != "main" || o.Turn != "turn-7" || parent.Provider != "claude-code" || parent.RuntimeRevision != 7 || parent.ObservedModel != "claude-opus-fixture" || a.Provider != "openai-codex" || a.Model != "gpt-fixture" || a.Effort != "medium" || b.Provider != "claude-code" || b.Status != "done" || c.Status != "failed" {
		t.Fatal("incorrect independent runtime/status metadata", o.Nodes)
	}
	for _, e := range o.Edges {
		if e.Owner != "Unreal Host" {
			t.Fatal("provider directly owns an edge", e)
		}
	}
	if a.ParentID != parent.ID || topologyNode(t, o, "agent:child-a/op:test-a").ParentID != a.ID {
		t.Fatal("operation/child ownership linkage lost")
	}
	data, err := json.Marshal(o, jsonv1.FormatDurationAsNano(true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "never-shown") {
		t.Fatal("raw/secret content leaked into projection")
	}
	// Changing the parent does not mutate already-created children or observed
	// model identities. A stale parent observation must not cross the switch.
	s.Selection = &sessionstore.RuntimeSelection{Version: 1, Provider: "openai-codex", Model: "gpt-new", Effort: "medium", Revision: 8}
	o = BuildOrchestration(s, u.Viewer, u.Now, 128)
	if topologyNode(t, o, "agent:main").ObservedModel != "" || topologyNode(t, o, "agent:main").Provider != "openai-codex" || topologyNode(t, o, "agent:child-b").Model != "sonnet" || topologyNode(t, o, "agent:child-a").RuntimeRevision != 2 {
		t.Fatal("parent switch overwrote child or reused an old observation")
	}
	// The opposite direction also displays a running Claude child under Codex.
	other := u.Viewer
	other.Rows = append([]viewer.Row(nil), u.Viewer.Rows...)
	other.Rows[2].Finish = nil
	other.Rows[2].ParentOperationStatus = operation.StatusReady
	if child := topologyNode(t, BuildOrchestration(s, other, u.Now, 128), "agent:child-b"); child.Provider != "claude-code" || child.Status != "running" {
		t.Fatal("Codex parent / Claude child projection", child)
	}
	afterRows, err := json.Marshal(u.Viewer.Rows, json.Deterministic(true), jsonv1.FormatDurationAsNano(true))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(u.Viewer.Rows[0].Selection)
	if !slices.Equal(beforeRows, afterRows) || !slices.Equal(before, after) {
		t.Fatal("read-only projection mutated its source")
	}
}

func TestOrchestrationNestedChildrenCycleDefenseAndBound(t *testing.T) {
	s, u := orchestrationFixture()
	u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: "child-a1", ParentID: "child-a", Depth: 2, ParentOperationStatus: operation.StatusAwaiting, Label: "nested task"})
	o := BuildOrchestration(s, u.Viewer, u.Now, 128)
	if topologyNode(t, o, "agent:child-a1").ParentID != "agent:child-a" {
		t.Fatal("nested lineage lost")
	}
	rows := topologyRows(o, 90, Theme{Plain: true})
	var nested string
	for _, r := range rows {
		if strings.Contains(r.content.plain(), "child-a1") {
			nested = r.content.plain()
		}
	}
	if !strings.Contains(nested, "│  └─") && !strings.Contains(nested, "│  ├─") {
		t.Fatal("nested connectors absent", nested)
	}
	u.Viewer.Rows = append(u.Viewer.Rows, viewer.Row{ID: "cycle-x", ParentID: "cycle-y"}, viewer.Row{ID: "cycle-y", ParentID: "cycle-x"})
	o = BuildOrchestration(s, u.Viewer, u.Now, 128)
	text := ""
	for _, r := range topologyRows(o, 90, Theme{}) {
		text += r.content.plain() + "\n"
	}
	if !strings.Contains(text, "lineage unknown") || strings.Count(text, "cycle-x") != 1 || strings.Count(text, "cycle-y") != 1 {
		t.Fatal("cycle did not fail safe", text)
	}
	for i := range 2000 {
		u.Viewer.Rows[0].Operations = append(u.Viewer.Rows[0].Operations, viewer.OperationRow{ID: operation.ID(fmt.Sprintf("op-%04d", i)), Tool: "read", Status: operation.StatusCompleted, Sequence: sessionstore.Sequence(i + 1)})
	}
	o = BuildOrchestration(s, u.Viewer, u.Now, 64)
	if len(o.Nodes) > 64 || o.Hidden == 0 || len(topologyRows(o, 80, Theme{})) > 64*6+2 {
		t.Fatal("unbounded topology", len(o.Nodes), o.Hidden)
	}
	if topologyNode(t, o, "agent:child-a/op:test-a").Status != "running" {
		t.Fatal("old operations displaced the active path")
	}
	_ = topologyNode(t, o, "agent:main/op:op-1999")
}

func TestOrchestrationUnknownDisconnectedResyncAndFailure(t *testing.T) {
	s, u := orchestrationFixture()
	u.Viewer.Rows[1].Selection = nil
	u.Viewer.Rows[1].NeedsResync = true
	u.Viewer.Rows[1].Problem = "raw-internal-body-never-shown"
	o := BuildOrchestration(s, u.Viewer, u.Now, 128)
	a := topologyNode(t, o, "agent:child-a")
	if a.RuntimeKnown || a.Provider != "" || a.Status != "unknown" || a.Problem != "resync required" {
		t.Fatal("unknown metadata guessed", a)
	}
	s.Connected, s.Status = false, "resyncing"
	o = BuildOrchestration(s, u.Viewer, u.Now, 128)
	if topologyNode(t, o, "agent:main").Status != "unknown" || topologyNode(t, o, "agent:child-a/op:test-a").Status != "unknown" || topologyNode(t, o, "agent:child-b").Status != "done" {
		t.Fatal("disconnection fabricated activity or erased canonical Finish")
	}
	s.Connected, s.Failure = true, "provider-internal-secret-never-shown"
	s.Status = "failed"
	s.Entries = append(s.Entries, Entry{Role: "error", Code: "external_reauth_required", Text: "private-error-never-shown"})
	o = BuildOrchestration(s, u.Viewer, u.Now, 128)
	if topologyNode(t, o, "agent:main").Problem != "auth required" {
		t.Fatal("safe auth status absent")
	}
	u.View = ViewOrchestration
	f := RenderFrame(s, u, 80, 24)
	if strings.Contains(frameText(f), "never-shown") {
		t.Fatal("Orchestration displayed body/error detail", frameText(f))
	}
}

func TestOrchestrationClaudePermissionDenialShowsOnlySafeStatus(t *testing.T) {
	for _, failure := range []string{
		`call model for turn "turn-id": Claude tool bridge: provider permission denied (decision_reason_type=asyncAgent; scope=unreal_tool_call)`,
		`call model for turn "turn-id": Claude Code: provider permission denied (decision_reason_type=unknown; scope=unknown)`,
		`call model for turn "turn-id": Claude tool bridge allowlist is inactive: organization requires managed permission rules (allowed=0/7)`,
	} {
		s, u := orchestrationFixture()
		s.Status, s.Failure = "failed", failure
		u.View = ViewOrchestration
		parent := topologyNode(t, BuildOrchestration(s, u.Viewer, u.Now, 128), "agent:main")
		if parent.Problem != "provider permission denied" {
			t.Fatal("lost closed provider failure", parent)
		}
		for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 30}} {
			for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
				u.Theme = theme
				frame := RenderFrame(s, u, size[0], size[1])
				text := frameText(frame)
				if len(frame.Lines) != size[1] || frame.CursorY != size[1]-1 || strings.Contains(text, "decision_reason_type") || strings.Contains(text, "turn-id") {
					t.Fatal("Orchestration leaked error details or displaced composer", size)
				}
			}
		}
	}
}

func TestOrchestrationOperationStatesSafeTargetsAndHighlight(t *testing.T) {
	s, u := orchestrationFixture()
	states := []operation.Status{operation.StatusAwaiting, operation.StatusReady, operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled, "invalid"}
	words := []string{"queued", "running", "done", "failed", "canceled", "unknown"}
	for i, status := range states {
		s.Operations = []operation.Operation{{ID: "parent-op", ToolName: "edit", Status: status, State: []byte(`{"TerminalError":"private-error-never-shown","arguments":"raw-args-never-shown"}`)}}
		s.Entries = append(s.Entries, Entry{Calls: []Receipt{{Name: "edit", Target: "render.go", Operations: []operation.ID{"parent-op"}}}})
		o := BuildOrchestration(s, u.Viewer, u.Now, 128)
		n := topologyNode(t, o, "agent:main/op:parent-op")
		if n.Status != words[i] || n.SafeTarget != "render.go" || n.Active != (i < 2) {
			t.Fatal("incorrect tool state", n)
		}
		rows := topologyRows(o, 90, Theme{})
		var text string
		var highlighted bool
		for _, r := range rows {
			text += r.content.plain() + "\n"
			if strings.Contains(r.content.plain(), "parent-op") {
				t.Fatal("internal operation ID used as tool title")
			}
			if strings.Contains(r.content.plain(), "edit") {
				highlighted = strings.Contains(r.content.paint(Theme{}), "\x1b[1m")
			}
		}
		if strings.Contains(text, "never-shown") || i < 2 && !highlighted {
			t.Fatal("raw state or active path styling", text)
		}
	}
}

func TestOrchestrationResponsiveViewsRailComposerAndThemes(t *testing.T) {
	s, u := orchestrationFixture()
	u.Viewer.Rows[1].Label = "猫👩🏽‍💻é parser inspection"
	u.Input = "猫👩🏽‍💻é\nsecond composer line"
	u.Cursor = len(u.Input)
	for _, view := range []ViewMode{ViewChat, ViewOrchestration, ViewSplit} {
		for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {119, 24}, {120, 24}, {159, 30}, {160, 40}, {220, 40}} {
			for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {Plain: true, ASCII: true}} {
				u.View, u.Theme = view, theme
				f := RenderFrame(s, u, size[0], size[1])
				exportFrameBounds(t, f, size[0], size[1])
				if f.View != view.effective(size[0]) || f.CursorY != size[1]-1 || len(f.Lines) != size[1] {
					t.Fatal("view/composer misplaced", f.View, size)
				}
				text := sgrRE.ReplaceAllString(frameText(f), "")
				if !utf8.ValidString(text) || strings.ContainsAny(text, "\x1b\r\a") {
					t.Fatal("invalid terminal text")
				}
				if f.View == ViewSplit {
					if !strings.Contains(sgrRE.ReplaceAllString(f.Lines[1], ""), "Chat") || !strings.Contains(sgrRE.ReplaceAllString(f.Lines[1], ""), "Orchestration") {
						t.Fatal("Split headings are not aligned")
					}
					if strings.Contains(text, "child agents") || !strings.Contains(text, "Chat") || !strings.Contains(text, "Orchestration") || !strings.Contains(text, "raw-response") {
						t.Fatal("Split duplicate rail or missing panes", text)
					}
					if strings.Count(text, "child-a") > 1 {
						t.Fatal("child duplicated in Split", text)
					}
				} else if f.View == ViewChat && size[0] >= 120 && !strings.Contains(text, "child agents") {
					t.Fatal("Chat lost existing child rail")
				}
				if f.View == ViewOrchestration && strings.Contains(text, "never-shown") {
					t.Fatal("conversation leaked into read-only topology", text)
				}
				if theme.Plain && sgrRE.MatchString(frameText(f)) {
					t.Fatal("plain emitted SGR")
				}
				if theme.NoColor {
					for _, match := range sgrRE.FindAllStringSubmatch(frameText(f), -1) {
						for _, code := range strings.Split(match[1], ";") {
							if code != "0" && code != "1" && code != "2" && code != "7" {
								t.Fatal("NO_COLOR color", code)
							}
						}
					}
				}
				if theme.ASCII && strings.ContainsAny(text, "├└│─▸✓✗⊘◌·↑↓›…") {
					t.Fatal("Unicode chrome in ASCII")
				}
			}
		}
	}
	if u.View != ViewSplit {
		t.Fatal("render resize mutated selected view")
	}
	for _, width := range []int{80, 120, 80, 160} {
		f := RenderFrame(s, u, width, 24)
		if f.View != ViewSplit.effective(width) || u.View != ViewSplit {
			t.Fatal("resize discarded Split preference")
		}
	}
}

func TestOrchestrationMetadataSecurityAndDeterminism(t *testing.T) {
	s, u := orchestrationFixture()
	s.Selection.Name = "access_token=synthetic-name-token"
	s.PendingSelection = &sessionstore.RuntimeSelection{Version: 1, Provider: "claude-code", Model: "opus", Name: "api_key=synthetic-pending-token"}
	u.Viewer.Rows[1].Label = "api_key=synthetic-token"
	u.Viewer.Rows[1].Operations[0].Target = "Authorization: Bearer synthetic-token"
	u.Viewer.Rows[2].Label = "safe task\x1b]52;c;terminal-control\a\u202e"
	u.Viewer.Rows[2].Failure = "credential-error-body-never-shown"
	u.Viewer.Rows[2].Activity = "agent private reasoning never-shown"
	u.Viewer.Rows[2].Finish.ChangedFiles = []string{"private-files-never-shown"}
	u.Viewer.Rows[2].Finish.Blockers = []string{"private-blockers-never-shown"}
	first := BuildOrchestration(s, u.Viewer, u.Now, 128)
	for range 5 {
		if !reflect.DeepEqual(first, BuildOrchestration(s, u.Viewer, u.Now, 128)) {
			t.Fatal("nondeterministic topology")
		}
	}
	data, err := json.Marshal(first, jsonv1.FormatDurationAsNano(true))
	if err != nil || strings.Contains(string(data), "synthetic-token") || strings.Contains(string(data), "never-shown") {
		t.Fatal("unsafe metadata projection", err)
	}
	u.View = ViewOrchestration
	frame := RenderFrame(s, u, 160, 40)
	if strings.Contains(frameText(frame), "synthetic-name-token") || strings.Contains(frameText(frame), "synthetic-pending-token") {
		t.Fatal("header/dock exposed protected runtime labels")
	}
	for _, r := range topologyRows(first, 80, Theme{Plain: true}) {
		if strings.ContainsAny(r.content.plain(), "\x1b\a\u202e") || uniseg.StringWidth(r.content.plain()) > 80 {
			t.Fatal("unsafe terminal metadata")
		}
	}
}

func TestOrchestrationScrollReachesEveryNodeAndSplitFollowsActivePath(t *testing.T) {
	s, u := orchestrationFixture()
	u.Viewer.Rows = u.Viewer.Rows[:1]
	for i := range 45 {
		u.Viewer.Rows[0].Operations = append(u.Viewer.Rows[0].Operations, viewer.OperationRow{ID: operation.ID(fmt.Sprint(i)), Tool: fmt.Sprintf("tool-%02d", i), Status: operation.StatusCompleted})
	}
	u.Viewer.Rows[0].Operations[42].Status = operation.StatusReady
	u.View = ViewOrchestration
	seen := map[string]bool{}
	for offset := range 60 {
		u.OrchestrationScroll = offset
		f := RenderFrame(s, u, 60, 20)
		for i := range 45 {
			if strings.Contains(frameText(f), fmt.Sprintf("tool-%02d", i)) {
				seen[fmt.Sprint(i)] = true
			}
		}
		if f.OrchestrationOffset > f.OrchestrationMax {
			t.Fatal("scroll outside bounded viewport")
		}
	}
	if len(seen) != 45 {
		t.Fatal("unreachable operation rows", len(seen))
	}
	u.View = ViewSplit
	f := RenderFrame(s, u, 120, 24)
	if !strings.Contains(frameText(f), "tool-42") || !strings.Contains(frameText(f), "parent main") {
		t.Fatal("Split lost active path or parent", frameText(f))
	}
}
