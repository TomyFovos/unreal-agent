package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestHeaderBandsOrderAndOmission(t *testing.T) {
	s, u := statsFixture("claude-code"), plainUI()
	s.ID = "long-session-name"
	s.Selection.Name = "Claude Opus 5.5"
	s.Selection.Effort = "max"
	s.Permissions = permission.ModeSnapshot{Enabled: true, Mode: permission.FullAccess}
	u.Parent = &viewer.Row{Elapsed: viewer.Duration{Known: true, Value: 7*time.Hour + 45*time.Minute}, Usage: viewer.Usage{Known: true, Input: 144_000_000, Output: 739_000}}
	parts := []string{"[long-session-name]", "[入力待ち 7h45m]", "[Full Access]", "[Claude Opus 5.5 · max]"}
	full := strings.Join(parts, " ")
	if got := header(s, u, layout{width: 200}).plain(); got != full {
		t.Fatalf("header=%q, want %q", got, full)
	}
	checks := []struct{ want string }{
		{strings.Join(parts[:3], " ")},
		{"[long-session-name] [入力待ち] [Full Access]"},
		{"[long-session-name] [入力待ち] [FULL]"},
		{"[long-se…on-name] [入力待ち] [FULL]"},
	}
	for _, check := range checks {
		width := textLine(check.want, normal).width()
		if got := header(s, u, layout{width: width}).plain(); got != check.want {
			t.Fatalf("width %d: got %q, want %q", width, got, check.want)
		}
	}
	for _, mode := range []permission.Mode{permission.Ask, permission.ReadOnly} {
		s.Permissions.Mode = mode
		short := "ASK"
		if mode == permission.ReadOnly {
			short = "RO"
		}
		got := header(s, u, layout{width: 31}).plain()
		if !strings.HasSuffix(got, "["+short+"]") {
			t.Fatalf("permission shortcut missing: %q", got)
		}
	}
	for _, gone := range []string{"unreal agent", "Split", "144", "739", "Permissions:", "connected"} {
		if strings.Contains(full, gone) {
			t.Fatalf("header exposes %q", gone)
		}
	}
}

func TestHeaderConnectionReplacesRuntimeBand(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	u.Theme = Theme{Depth: ColorTrue, Still: true}
	u.Parent = &viewer.Row{Elapsed: viewer.Duration{Known: true, Value: 42 * time.Second}}
	for _, tc := range []struct {
		status  string
		offline time.Duration
		want    string
		role    colorRole
	}{
		{"connecting", 0, "▸ 接続中", bandWarn},
		{"disconnected", time.Second, "▸ 再接続中", bandWarn},
		{"gap/disconnect; resyncing", time.Second, "▸ 再同期中", bandWarn},
		{"disconnected", 3 * time.Second, "✗ 切断", bandBad},
	} {
		s.Connected, s.Status, s.OfflineSince = false, tc.status, u.Now.Add(-tc.offline)
		got := header(s, u, layout{width: 200})
		if got.plain() != string(s.ID)+" "+tc.want || strings.Contains(got.plain(), "42s") {
			t.Fatalf("connection %s: %q", tc.status, got.plain())
		}
		if got[len(got)-1].style != visualStyle(tc.role) {
			t.Fatal("connection has wrong band color", tc.status)
		}
	}
	for _, tc := range []struct {
		running bool
		failure string
		want    string
		role    colorRole
	}{{true, "", "入力待ち 42s", bandNeutral}, {false, "", "▪ 停止", bandNeutral}, {false, "failure", "✗ 失敗", bandBad}} {
		s.Connected, s.Running, s.Failure, s.Status = true, tc.running, tc.failure, "connected"
		got := header(s, u, layout{width: 200})
		if got.plain() != string(s.ID)+" "+tc.want || got[len(got)-1].style != visualStyle(tc.role) {
			t.Fatal("runtime band", got.plain())
		}
	}
}

func TestAttentionBandPriorityCountAndZeroRows(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	l := makeLayout(160, 40)
	if got := attentionBand(s, u, l); len(got) != 0 {
		t.Fatal("unconditional attention row", plainRows(got))
	}
	base := RenderFrame(s, u, 160, 40)
	s.Entries = []Entry{{Role: "agent", Text: "【要確認】古い確認"}, {Role: "you", Text: "返答"}, {Role: "agent", Text: "【要確認】F の失敗を判断してください\n2行目"}}
	check := func(want string, role colorRole) {
		t.Helper()
		got := attentionBand(s, u, l)
		if len(got) != 1 || got[0].plain() != want || got[0][0].style != visualStyle(role) {
			t.Fatalf("attention=%q, want %q", plainRows(got), want)
		}
		f := RenderFrame(s, u, 160, 40)
		if f.Lines[35] != want || !strings.Contains(f.Lines[36], "^D detach") || f.ConversationHeight != base.ConversationHeight-1 || f.CursorY != base.CursorY {
			t.Fatal("band is not one row above keybar", frameText(f))
		}
	}
	check("[▲ 要確認  F の失敗を判断してください  ↑ 会話で確認]", bandWarn)
	// A foreground question remains visible as child notifications and their
	// background replies arrive, including replies with no further action.
	s.Entries = append(s.Entries,
		Entry{Role: "peer", Text: "child ended"},
		Entry{Role: "agent", Text: "遅れて届いた通知、追加の対応なし"},
		Entry{Role: "child", Text: "別の子の通知"},
		Entry{Role: "agent", Text: "確認しました"})
	check("[▲ 要確認  F の失敗を判断してください  ↑ 会話で確認]", bandWarn)
	s.Permissions.Pending = []permission.Pending{{ID: "p1"}, {ID: "p2"}}
	check("[▲ 承認待ち 2件  /permissions +1]", bandWarn)
	s.Failure = "生成に失敗しました\n内部の2行目"
	check("[✗ 応答を取得できませんでした。/resume で再開するか、/model で切り替えてください。 +2]", bandBad)
	s.Failure = ""
	for _, kind := range []string{"permission-once", "permissions"} {
		u.Picker = &Picker{Kind: kind}
		got := attentionBand(s, u, l)
		if len(got) != 1 || got[0].plain() != "[▲ 要確認  F の失敗を判断してください  ↑ 会話で確認]" {
			t.Fatal("open approval picker still counted", plainRows(got))
		}
	}
	u.Picker = nil
	s.Permissions.Pending = nil
	s.Entries = append(s.Entries, Entry{Role: "you", Text: "判断しました"})
	if len(attentionBand(s, u, l)) != 0 || len(stateDock(s, u, l)) != 0 {
		t.Fatal("answered attention remains")
	}
	s.Running = false
	if len(attentionBand(s, u, l)) != 0 || len(stateDock(s, u, l)) != 0 {
		t.Fatal("stopped session still takes a dock row")
	}
}

func TestGaugeContextRateColorsAndOmission(t *testing.T) {
	s, u := statsFixture("claude-code"), plainUI()
	s.LastGeneration.Tokens.Input = 394_000
	s.LastGeneration.Tokens.Output = 138
	s.LastGeneration.Source = "client_clock"
	s.Selection.ContextWindow = 1_000_000
	s.CacheLatest = viewer.Usage{Known: true, Input: 100, CachedInput: 98}
	u.Parent = &viewer.Row{Usage: viewer.Usage{Known: true, Input: 144_400_000, Output: 738_800}}
	for _, rate := range []int64{0, 23, 46, 69, 92, 115, 138, 138} {
		stats := *s.LastGeneration
		stats.Tokens.Output = rate
		s.SpeedHistory = append(s.SpeedHistory, stats)
	}
	context := "文脈 ▕█████░░░░░░░▏394k (39%)"
	speed := "速度 ▁▂▃▄▅▆▇▇ ~69.0 tok/s"
	cache := "キャッシュ ▕██████████▏98.00%"
	for _, want := range []string{
		context + " · " + speed + " · " + cache,
		context + " · " + speed,
		context + " · 速度 ~69.0 tok/s",
		"文脈 394k (39%) · 速度 ~69.0 tok/s",
	} {
		if got := statisticsBar(s, u, layout{width: textLine(want, normal).width()}).plain(); got != want {
			t.Fatalf("gauge got %q, want %q", got, want)
		}
	}
	if got := statisticsBar(s, u, layout{width: 300}).plain(); strings.Contains(got, "累計") || got != context+" · "+speed+" · "+cache {
		t.Fatal("wide gauge must omit totals", got)
	}
	s.Selection.ContextWindow = 0
	got := statisticsBar(s, u, layout{width: 200}).plain()
	if !strings.HasPrefix(got, "文脈 394k · ") {
		t.Fatal("unknown context window fabricated a bar/percentage", got)
	}
	s.LastGeneration.Source = "provider_api"
	got = statisticsBar(s, u, layout{width: 200}).plain()
	if strings.Contains(got, "~") || !strings.Contains(got, "69.0 tok/s") {
		t.Fatal("API speed marked estimated", got)
	}
	for _, tc := range []struct {
		input int64
		role  colorRole
	}{{699, stateMain}, {700, stateStall}, {899, stateStall}, {900, stateBad}} {
		s.Selection.ContextWindow, s.LastGeneration.Tokens.Input = 1000, tc.input
		got := statisticsBar(s, u, layout{width: 200})
		found := false
		for _, g := range got {
			if g.text == "▕" {
				found = true
				if g.style.fg != tc.role {
					t.Fatalf("context %d uses %s, want %s", tc.input, g.style.fg, tc.role)
				}
				break
			}
		}
		if !found {
			t.Fatal("context bar absent")
		}
	}
}

func TestGaugeUsesLastContextAndSummedCacheUsage(t *testing.T) {
	s, u := statsFixture("claude-code"), plainUI()
	s.LastGeneration.Tokens.Input = 700_000
	s.LastGeneration.ContextTokens, s.LastGeneration.ContextKnown = 394_000, true
	s.Selection.ContextWindow = 1_000_000
	s.CacheLatest = viewer.Usage{Known: true, Input: 700_000, CachedInput: 350_000}
	for _, progress := range []bool{false, true} {
		if progress {
			s.Progress = &host.Progress{Mode: "streaming", Statistics: s.LastGeneration}
		}
		got := statisticsBar(s, u, layout{width: 160}).plain()
		if !strings.Contains(got, "文脈 ▕█████░░░░░░░▏394k (39%)") || !strings.Contains(got, "50.00%") || strings.Contains(got, "700k") {
			t.Fatal("context must use the last call; cache keeps summed usage", got)
		}
	}
}

func TestGenerationHistoryEightSamplesReplayAndWaits(t *testing.T) {
	m := NewModel("s")
	v := host.View{Generation: "g", Running: true}
	for i := 1; i <= 10; i++ {
		stats := &llm.GenerationStatistics{Model: "fixture-model", Source: "provider_api", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{Input: 394_000, InputKnown: true, Output: int64(i), OutputKnown: true}}
		v.History.Items = append(v.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(i), Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Model: stats.Model, Generation: stats}}})
	}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	s := m.Snapshot()
	if len(s.SpeedHistory) != 8 {
		t.Fatal("history size", len(s.SpeedHistory))
	}
	for i, stats := range s.SpeedHistory {
		if stats.Tokens.Output != int64(i+3) {
			t.Fatal("history order/replayed page", s.SpeedHistory)
		}
	}
	s.SpeedHistory[0].Tokens.Output = 999
	if m.Snapshot().SpeedHistory[0].Tokens.Output != 3 {
		t.Fatal("history aliases Model")
	}
	// Identical counters from a distinct response still occupy a new slot.
	v.History.Items = append(v.History.Items, host.HistoryItem{Sequence: 11, Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Model: "fixture-model", Generation: s.LastGeneration}}})
	if err := m.apply(v); err != nil {
		t.Fatal(err)
	}
	s = m.Snapshot()
	if s.SpeedHistory[0].Tokens.Output != 4 || s.SpeedHistory[7].Tokens.Output != 10 {
		t.Fatal("equal generation omitted", s.SpeedHistory)
	}
	s.Operations = []operation.Operation{{ID: "tool", Status: operation.StatusAwaiting}}
	s.Permissions.Pending = []permission.Pending{{ID: "tool"}}
	u := plainUI()
	before := statisticsBar(s, u, makeLayout(160, 40)).plain()
	u.Now = u.Now.Add(time.Hour)
	if after := statisticsBar(s, u, makeLayout(160, 40)).plain(); after != before {
		t.Fatal("wait changed speed", before, after)
	}
	// Streaming measurements update the provisional last bar without appending
	// a sample on every progress event.
	m.setProgress(&host.Progress{Epoch: 1, Mode: "streaming", Statistics: s.LastGeneration})
	if len(m.Snapshot().SpeedHistory) != 8 || generationBars(m.Snapshot(), u.Theme).width() != 9 {
		t.Fatal("stream grew speed history")
	}
	fresh := NewModel("s")
	if err := fresh.apply(v); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fresh.Snapshot().SpeedHistory) != fmt.Sprint(s.SpeedHistory) {
		t.Fatal("reattach history differs")
	}
}

func TestStatusDockJapaneseAndPulseTools(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.PendingSelection = &sessionstore.RuntimeSelection{Provider: "claude-code", Model: "opus", Effort: "max"}
	if got := plainRows(stateDock(s, u, makeLayout(160, 40))); !strings.Contains(got, "次のターンから: ") {
		t.Fatal(got)
	}
	s.Operations = []operation.Operation{{ToolName: "Read", Status: operation.StatusReady}, {ToolName: "Write", Status: operation.StatusReady}}
	if got := pulse(s, u, makeLayout(160, 40)).plain(); !strings.Contains(got, "ツール2件 実行中") {
		t.Fatal(got)
	}
}

func TestCompactPulseAndConversationRecord(t *testing.T) {
	s, u := idleSnapshot(), plainUI()
	s.Progress = &host.Progress{Mode: "compacting", Attempt: 1}
	for _, background := range []bool{false, true} {
		if background {
			s.Entries = append(s.Entries, Entry{Role: "peer", Text: "child ended"})
		}
		if got := pulse(s, u, makeLayout(160, 40)).plain(); !strings.Contains(got, "会話を圧縮中") {
			t.Fatal("missing compact phase", got)
		}
		if got := header(s, u, makeLayout(160, 40)).plain(); !strings.Contains(got, u.Theme.spinner(u.Now.UnixMilli())+" 会話を圧縮中") {
			t.Fatal("header missing compact spinner", got)
		}
		card := workCard{main: true, now: mainCardPulse(s, viewer.Row{}, nil, u.Now)}
		if got := plainRows(cardRows(card, 80, u, false)); !strings.Contains(got, u.Theme.spinner(u.Now.UnixMilli())+" 会話を圧縮中") {
			t.Fatal("main card missing compact spinner", got)
		}
	}
	record := llm.ConversationRecord{Provider: "claude", BeforeTokens: 250_000, InputTokens: 12_000, InputKnown: true, Compacted: true}
	item := host.HistoryItem{Sequence: 7, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Kind: "provider_conversation", Conversation: &record}}}
	entries := entriesFor(item)
	if len(entries) != 1 || entries[0].Role != "host" || entries[0].Sequence != 7 || entries[0].Text != "会話を圧縮しました: 250000 → 12000 トークン" {
		t.Fatal("compact record projection", entries)
	}
	record.InputKnown = false
	if got := entriesFor(item)[0].Text; !strings.Contains(got, "→ 不明") {
		t.Fatal("invented post-compact usage", got)
	}
	record.Compacted, record.Error = false, "synthetic failure"
	if got := entriesFor(item)[0].Text; !strings.Contains(got, "失敗しました: synthetic failure") {
		t.Fatal("missing compact error", got)
	}
	record.Interrupted, record.Error = true, ""
	if got := entriesFor(item)[0].Text; got != "会話の圧縮を中断しました" {
		t.Fatal("missing compact interruption", got)
	}
}

func TestBandsAndGaugeWidthAcrossColorModes(t *testing.T) {
	s, u := visualWorkScene()
	s.Failure = strings.Repeat("失敗の理由 👩🏽‍💻 ", 20) + "\n2行目"
	for _, theme := range []Theme{{Depth: ColorTrue}, {Depth: Color256}, {Depth: Color16}, {NoColor: true}, {Plain: true}, {ASCII: true}} {
		u.Theme = theme
		for width := 1; width <= 215; width++ {
			l := makeLayout(width, 40)
			rows := []line{header(s, u, l), statisticsBar(s, u, l)}
			rows = append(rows, attentionBand(s, u, l)...)
			for _, row := range rows {
				painted := row.paint(theme)
				plain := sgrRE.ReplaceAllString(painted, "")
				if got := textLine(plain, normal).width(); got > width-1 {
					t.Fatalf("theme=%+v width=%d row width=%d: %q", theme, width, got, plain)
				}
				if theme.Plain && sgrRE.MatchString(painted) {
					t.Fatal("Plain contains SGR")
				}
				if theme.ASCII && strings.ContainsAny(plain, "▁▂▃▄▅▆▇█░▕▏●✗▸") {
					t.Fatal("ASCII symbols", plain)
				}
			}
			suffix := "+2"
			if theme.NoColor || theme.Plain {
				suffix += "]"
			}
			if width >= 45 && !strings.HasSuffix(rows[2].plain(), suffix) {
				t.Fatal("narrow attention lost count", rows[2].plain())
			}
		}
	}
	if got := generationBars(Snapshot{SpeedHistory: []llm.GenerationStatistics{
		{Source: "provider_api", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{OutputKnown: true, Output: 1}},
		{Source: "provider_api", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{OutputKnown: true, Output: 2}},
		{Source: "provider_api", DurationMicros: 1_000_000, Tokens: llm.TokenCounts{OutputKnown: true, Output: 6}},
	}}, Theme{ASCII: true}).plain(); got != ":-# " {
		t.Fatal("ASCII speed levels", got)
	}
}
