package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestLoginShowsSubscriptionInstructionsWithoutRequestingKeys(t *testing.T) {
	f, keys, out := newFake(), make(chan Key, 8), &screenObserver{}
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &Theme{Plain: true}})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "入力待ち") })
	keys <- Key{Text: "/login"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "codex login") && strings.Contains(out.text(), "claude を起動")
	})
	for _, gone := range []string{"PROVIDER ID", "API key", "store key", "private:"} {
		if strings.Contains(out.text(), gone) {
			t.Fatal(gone, out.text())
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins != 0 || len(f.inputs) != 0 {
		t.Fatal("login requested a key or wrote conversation")
	}
}

func TestHostRecordsDoNotAppearInConversation(t *testing.T) {
	for _, kind := range []string{"configuration", "project_instructions", "runtime_selection", "runtime_applied", "context", "unknown_record"} {
		selection := &sessionstore.RuntimeSelection{Model: "model", Name: "表示モデル", Effort: "high"}
		entries := entriesFor(host.HistoryItem{Data: sessionstore.HostRecord{Kind: kind, Selection: selection}})
		if len(entries) != 0 {
			t.Fatal(kind, entries)
		}
	}
}

func TestFailureGuidanceKeepsDetailsInAnalysis(t *testing.T) {
	for _, provider := range []string{"openai-codex", "claude"} {
		s, u := idleSnapshot(), plainUI()
		s.Selection = &sessionstore.RuntimeSelection{Provider: provider}
		s.Failure = `call model for turn "internal-turn": provider: unauthenticated stage=http_status status=401 reason=invalid_token`
		s.Entries = []Entry{{Role: "error", Code: "external_reauth_required", Text: s.Failure}}
		expected := "codex login"
		if provider == "claude" {
			expected = "claude を起動して /login"
		}
		text := chatText(s, u, 160) + plainRows(attentionBand(s, u, makeLayout(200, 40)))
		for _, want := range []string{expected, "/model", "/resume"} {
			if !strings.Contains(text, want) {
				t.Fatal(want, text)
			}
		}
		for _, gone := range []string{"internal-turn", "stage=", "401", "external_reauth_required"} {
			if strings.Contains(text, gone) {
				t.Fatal(gone, text)
			}
		}
		m := NewModel("s")
		m.state = s
		m.diagnostic(errors.New("transport_failed stage=transport token=synthetic-secret"))
		report := m.Analysis(time.Now(), nil)
		text = ""
		for _, e := range report.Errors.Recent {
			text += e.Name
		}
		if !strings.Contains(text, "stage=http_status") || strings.Contains(text, "synthetic-secret") {
			t.Fatal("diagnostic lost/leaked", text)
		}
	}
}

func TestPermissionPickerExplainsModesAndHidesIdentifiers(t *testing.T) {
	s := Snapshot{ID: "internal-session", Permissions: permission.ModeSnapshot{Enabled: true, Mode: permission.Ask, Pending: []permission.Pending{{ID: "internal-operation", Tool: "Bash", Scope: "go test ./..."}}}}
	p := permissionPicker(s)
	for i, want := range []string{"読むだけ", "書き込みと実行の前に確認", "確認なし。OS の隔離ではありません", "go test ./..."} {
		if !strings.Contains(p.Options[i], want) {
			t.Fatal(p.Options)
		}
	}
	confirmation := operationPermissionConfirmation(s, s.Permissions.Pending[0])
	text := strings.Join(append(append(p.Options, confirmation.Instructions...), confirmation.Footer...), " ")
	m := NewModel(s.ID)
	m.state = s
	details := m.Analysis(time.Now(), nil)
	found := false
	for _, e := range details.Timeline {
		found = found || strings.Contains(e.Name, "internal-operation")
	}
	if !found {
		t.Fatal("approval id is missing from diagnostics")
	}
	if strings.Contains(text, "internal-") || confirmation.Selection != 0 {
		t.Fatal(text)
	}
}

func TestAnalysisToolCapabilityDoesNotClaimMCP(t *testing.T) {
	m := NewModel("s")
	record := sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"claude","model":{"id":"opus"}},"ReasoningEffort":"high"}`)}
	view := host.View{Generation: "g", History: host.HistoryPage{Items: []host.HistoryItem{{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: record}}}}
	if err := m.apply(view); err != nil {
		t.Fatal(err)
	}
	m.setCapabilities(map[string]modelcatalog.Capabilities{"claude": {Tools: true}})
	r := m.Analysis(time.Now(), nil)
	if r.ToolCapability != "Tools available" || strings.Contains(strings.Join(analysis.Lines(r, "Overview", false), "\n"), "MCP") {
		t.Fatal(r.ToolCapability)
	}
}

func TestSessionDiagnosticSurvivesResume(t *testing.T) {
	m := NewModel("s")
	if err := m.apply(host.View{Generation: "g", Failure: "provider: unauthenticated status=401"}); err != nil {
		t.Fatal(err)
	}
	if err := m.apply(host.View{Generation: "resumed", Running: true}); err != nil {
		t.Fatal(err)
	}
	r := m.Analysis(time.Now(), nil)
	found := false
	for _, e := range r.Errors.Recent {
		found = found || strings.Contains(e.Name, "status=401")
	}
	if !found {
		t.Fatal("resume discarded diagnostic cause")
	}
}

func TestFailedTurnIsVisibleWhileSessionKeepsRunning(t *testing.T) {
	m := NewModel("s")
	const reason = `call model for turn "turn-1": Claude Code: turn_failed: You've hit your limit`
	items := []host.HistoryItem{
		{Sequence: 1, Kind: sessionstore.ItemInput, Data: inbox.Input{ID: "user", Kind: inbox.InputExternal, Payload: []byte(`"continue"`)}},
		{Sequence: 2, Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular}},
		{Sequence: 3, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: sessionstore.HostActivity, Activity: &llm.ActivityRecord{Kind: llm.ActivityModelFailure, ID: "turn-1", Reason: reason}}},
	}
	v := host.View{Generation: "g", Running: true, History: host.HistoryPage{Items: items}}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if !s.Running || s.WaitingForModel || len(s.Entries) != 2 || s.Entries[1].Role != "error" || s.Entries[1].TurnID != "turn-1" {
		t.Fatal("failed turn projection", s)
	}
	if text := chatText(s, plainUI(), 160); !strings.Contains(text, "利用上限に達しました") || !strings.Contains(text, "入力を送り直す") || strings.Contains(text, "/resume") {
		t.Fatal(text)
	}
	diagnostic := m.Analysis(time.Now(), nil).Errors.Recent
	if len(diagnostic) != 1 || diagnostic[0].Name != reason {
		t.Fatal("failed turn cause was lost", diagnostic)
	}
}

func TestCacheGaugeKeepsTwoDecimalPlaces(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.CacheLatest = viewer.Usage{Known: true, Input: 10000, CachedInput: 9837}
	text := statisticsBar(s, u, makeLayout(160, 40)).plain()
	if !strings.Contains(text, "キャッシュ ▕██████████▏98.37%") || strings.Contains(text, "cache ") {
		t.Fatal(text)
	}
}
