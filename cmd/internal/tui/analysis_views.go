package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

var analysisLabels = map[string]struct{ ja, enQuestion, jaQuestion string }{
	"Overview":                     {"概要", "Is this Session healthy right now?", "このセッションは今、正常に動いているか？"},
	"Usage":                        {"トークン使用量", "What consumed tokens, and where is waste?", "何がトークンを消費し、どこに無駄があるか？"},
	analysis.SubscriptionUsageView: {"サブスクリプション利用枠", "When will each allowance run out?", "各利用枠はいつ使い切るか？"},
	"Context":                      {"文脈", "Is the 3-layer window healthy and cheap?", "3層の文脈ウィンドウは正常で、無駄が少ないか？"},
	"Agents":                       {"子エージェント", "Which children cost the most, which failed, which are stuck?", "どの子が多く消費し、失敗し、停滞しているか？"},
	"Tools":                        {"ツール", "Where does tool time go and what fails?", "どのツールに時間がかかり、何が失敗するか？"},
	"Time":                         {"時間", "Where did wall-clock time go?", "経過時間は何に使われたか？"},
	"Errors":                       {"エラー", "What is failing now?", "いま何が失敗しているか？"},
	"Turns":                        {"ターン", "Which turns used the most input and output?", "どのターンが最も多く入力・出力を使ったか？"},
	"Export":                       {"書き出し", "How can I save analysis data?", "分析データをどう保存するか？"},
}

func analysisJapanese(getenv func(string) string) bool {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if locale := getenv(key); locale != "" {
			return strings.HasPrefix(strings.ToLower(locale), "ja")
		}
	}
	return false
}

func analysisViewName(view string, ja bool) string {
	if name := analysisLabels[view].ja; ja && name != "" {
		return name
	}
	return view
}

func analysisQuestion(view string, ja bool) string {
	label := analysisLabels[view]
	if ja {
		return label.jaQuestion
	}
	return label.enQuestion
}

func analysisRows(r analysis.Report, target analysisTarget, theme Theme) []line {
	p := analysisPainter{width: target.width, theme: theme, ja: target.ja}
	d := r.Dashboard
	if d == nil {
		d = &analysis.Dashboard{}
	}
	complete := !r.Partial && (r.Tree == nil || !r.Tree.Partial)
	if !complete {
		p.add(p.tr("Partial history · observed data", "履歴の一部 · 観測済みのデータ"), warningStyle)
	}
	switch target.view {
	case "Overview":
		p.overview(r, d, complete)
	case "Usage":
		p.usageViews(r, d, complete)
	case analysis.SubscriptionUsageView:
		p.subscriptions(r)
	case "Context":
		p.contextView(r, d.Context)
	case "Agents":
		p.agents(r.At, d)
	case "Tools":
		p.tools(d, r.ByTool)
	case "Time":
		if target.raw {
			p.section("Raw events", "イベント記録")
			if len(r.Timeline) == 0 {
				p.add(absentMeasure, meta)
			}
			for _, event := range r.Timeline {
				at := absentMeasure
				if !event.At.IsZero() {
					at = displayTime(event.At, "01/02 15:04:05")
				}
				text := at + "  " + event.Kind + "  " + safeDiagnostic(event.Name)
				if event.Question != "" {
					p.rows = append(p.rows, wrapLine(textLine(text+"  "+safeDiagnostic(event.Question), normal), p.width, false, 2)...)
				} else {
					p.add(text, normal)
				}
			}
		} else {
			p.timeView(r, d, complete)
		}
	case "Errors":
		p.errors(r, d, complete)
	case "Turns":
		p.turns(r)
	}
	return p.rows
}

func (p *analysisPainter) overview(r analysis.Report, d *analysis.Dashboard, complete bool) {
	state, st := absentMeasure, meta
	switch d.State {
	case "running":
		state, st = p.tr("Running", "稼働中"), successStyle
	case "stopped":
		state = p.tr("Stopped", "停止")
	case "failed":
		state, st = p.tr("Session failed", "セッション失敗"), failureStyle
	}
	switch {
	case d.Approval:
		state, st = p.tr("Waiting for approval", "承認待ち"), warningStyle
	case d.State == "usage_limit_wait" || r.UsageRetryAt.After(r.At):
		state, st = p.tr("Usage-limit wait until ", "利用制限の解除待ち ")+analysisTime(r.UsageRetryAt, r.At), warningStyle
	}
	p.add(state+p.severity(st), st)
	if r.Selection != nil && r.Selection.Provider == "claude-code" && r.ToolBridgeStatus == "blocked_by_policy" {
		p.add(p.tr("Tool Bridge blocked by managed policy", "管理ポリシーにより Tool Bridge が利用不可"), warningStyle)
	}

	var sessionTokens, lastHour analysis.TokenMix
	for _, u := range d.Usage {
		sessionTokens.Add(u.Usage)
		if recentHour(u.At, r.At) {
			lastHour.Add(u.Usage)
		}
	}
	p.cacheGauge(p.tr("Cache read 1h", "キャッシュ読取 1h"), 20, lastHour.Cache.Percent())
	p.cacheGauge(p.tr("Cache read Session", "キャッシュ読取 全体"), 20, sessionTokens.Cache.Percent())

	tokens, _ := analysis.HourlyTokens(d.Usage, r.CreatedAt, r.At, complete, nil)
	p.tokenGraph(p.tr("Weighted tokens/h", "加重トークン/h"), tokens)

	p.section("Remaining allowance", "利用枠の残り")
	shown := false
	for _, w := range r.SubscriptionUsage {
		if w.UsedPercent != nil || w.State == "stale" {
			p.allowanceGauge(w, r.At, 22)
			shown = true
		}
	}
	if !shown {
		p.add(absentMeasure, meta)
	}
	p.failureBars(r, d, complete, false)
	p.activeChildren(d, complete)
}

func (p *analysisPainter) tokenGraph(title string, series analysis.TokenTrend) {
	projection := absentMeasure
	if len(series.Projection) != 0 {
		projection = tokensText(series.Projection[len(series.Projection)-1].Value) + "/h"
	}
	p.add(fmt.Sprintf(p.tr("%s  ·  EMA %.2f  ·  Next %dh %s", "%s · EMA %.2f · 次の%d時間 %s"), title, analysis.EMAAlpha, analysis.TokenForecastHours, projection), strong)
	p.chart(graph{height: 4, format: tokensText, series: []chartSeries{
		{points: series.Trend, st: liveStyle, mark: '=', dotted: true},
		{points: series.Projection, st: warningStyle, mark: '.', dotted: true},
		{points: series.Hourly, st: youStyle, mark: '#'},
	}})
}

func (p *analysisPainter) cacheGauge(label string, width int, value analysis.Metric) {
	st := cacheStyle(value)
	p.gauge(label, width, value, metricText(value, percentText)+p.severity(st), st)
}

type usageGroup struct {
	key   analysis.ModelKey
	label string
	mix   analysis.TokenMix
}

func compareUsage(a, b analysis.Metric) int {
	return cmp.Or(cmp.Compare(knownOrder(b.Known), knownOrder(a.Known)), cmp.Compare(b.Value, a.Value))
}

func knownOrder(known bool) int {
	if known {
		return 1
	}
	return 0
}

func usageGroups(d *analysis.Dashboard) (models, roles []usageGroup, children map[session.ID]analysis.TokenMix, total analysis.TokenMix) {
	modelTotals := make(map[analysis.ModelKey]analysis.TokenMix)
	roleTotals := make(map[string]analysis.TokenMix)
	children = make(map[session.ID]analysis.TokenMix)
	for _, u := range d.Usage {
		total.Add(u.Usage)
		model := analysis.ModelKey{Provider: u.Provider, Model: u.Model}
		m := modelTotals[model]
		m.Add(u.Usage)
		modelTotals[model] = m
		roleName := knownLabel(u.Role)
		role := roleTotals[roleName]
		role.Add(u.Usage)
		roleTotals[roleName] = role
		child := children[u.Session]
		child.Add(u.Usage)
		children[u.Session] = child
	}
	names := make(map[string]int)
	for key := range modelTotals {
		names[key.Model]++
	}
	for key, mix := range modelTotals {
		label := knownLabel(key.Model)
		if names[key.Model] > 1 && key.Provider != "" {
			label += " / " + key.Provider
		}
		models = append(models, usageGroup{key, label, mix})
	}
	for role, mix := range roleTotals {
		roles = append(roles, usageGroup{label: role, mix: mix})
	}
	sortGroups := func(groups []usageGroup) {
		slices.SortFunc(groups, func(a, b usageGroup) int {
			return cmp.Or(compareUsage(a.mix.Weighted, b.mix.Weighted), cmp.Compare(a.label, b.label))
		})
	}
	sortGroups(models)
	sortGroups(roles)
	return
}

func tokenParts(m analysis.TokenMix) []barPart {
	return []barPart{
		{m.Read.Value, "█", "#", successStyle}, {m.Write.Value, "▓", "=", warningStyle},
		{m.Uncached.Value, "▒", "+", youStyle}, {m.UnsplitInput.Value, "▰", "~", youStyle}, {m.Output.Value, "░", "-", liveStyle},
	}
}

func partsTotal(parts []barPart) float64 {
	var total float64
	for _, part := range parts {
		total += part.value
	}
	return total
}

func (p *analysisPainter) usageViews(r analysis.Report, d *analysis.Dashboard, complete bool) {
	models, roles, children, total := usageGroups(d)
	p.add(p.tr("R cache read  W cache write  U uncached  O output", "R キャッシュ読取  W 書込  U 未キャッシュ  O 出力"), meta)
	if total.UnsplitInput.Known {
		p.add(p.tr("N non-read input · write/uncached split unknown", "N 読取以外の入力 · 書込と未キャッシュの内訳不明"), meta)
	}
	for i, groups := range [][]usageGroup{models, roles} {
		p.blank()
		if i == 0 {
			p.section("By model · share of Session weighted tokens", "モデル別 · 全体の加重トークンに占める割合")
		} else {
			p.section("By role · share of Session weighted tokens", "役割別 · 全体の加重トークンに占める割合")
		}
		if len(groups) == 0 {
			p.add(absentMeasure, meta)
		}
		for _, row := range groups {
			p.usageGroup(row, total.Weighted)
		}
	}
	p.blank()
	p.section("Top children · share of Session weighted tokens", "消費の多い子 · 全体の加重トークンに占める割合")
	ranked := rankedChildren(d.Children, children)
	ranked = slices.DeleteFunc(ranked, func(child analysis.ChildSignal) bool { return !children[child.ID].Weighted.Known })
	if len(ranked) == 0 {
		p.add(absentMeasure, meta)
	}
	for _, child := range ranked[:min(5, len(ranked))] {
		mix := children[child.ID]
		model := knownLabel(child.Model)
		title := p.titleCell(analysisChildTitle(child), p.width-uniseg.StringWidth(model)-2, strong)
		p.rows = append(p.rows, joined(title, textLine("  "+model, strong)).clip(p.width))
		share := ratioMetric(mix.Weighted, total.Weighted)
		cache := mix.Cache.Percent()
		p.barRow("  ", 2, metricText(mix.Weighted, tokensText)+"  "+metricText(share, percentText)+"  "+p.tr("cache ", "キャッシュ ")+metricText(cache, percentText)+p.severity(cacheStyle(cache)), cacheStyle(cache), total.Weighted.Value,
			barPart{mix.Weighted.Value, "█", "#", liveStyle})
	}
	p.blank()
	p.section("Cache writes · first request after a boundary vs other", "キャッシュ書込 · 境界直後とその他")
	first, other := d.BoundaryWrites, d.OtherWrites
	writes := analysis.Metric{Known: first.Known && other.Known, Value: first.Value + other.Value, Unknown: max(first.Unknown, other.Unknown)}
	var scale float64
	if writes.Known {
		scale = writes.Value
	}
	p.barRow("", 0, metricText(first, tokensText)+" / "+metricText(other, tokensText)+"  "+metricText(ratioMetric(first, writes), percentText), normal, scale,
		barPart{first.Value, "▓", "=", warningStyle}, barPart{other.Value, "▒", "+", youStyle})
	p.blank()
	p.section("Hourly weighted tokens by model · dotted forecast", "モデル別の加重トークン/h · 点線は予測")
	for _, model := range models {
		series, _ := analysis.HourlyTokens(d.Usage, r.CreatedAt, r.At, complete, &model.key)
		p.tokenGraph(model.label, series)
	}
	if len(models) == 0 {
		p.add(absentMeasure, meta)
	}
}

func (p *analysisPainter) usageGroup(row usageGroup, total analysis.Metric) {
	m := row.mix
	cache := m.Cache.Percent()
	share := ratioMetric(m.Weighted, total)
	suffix := metricText(m.Weighted, tokensText) + "  " + metricText(share, percentText) + "  " + p.tr("cache ", "読取 ") + metricText(cache, percentText) + p.severity(cacheStyle(cache))
	p.barRow(row.label, max(uniseg.StringWidth(row.label)+1, min(36, p.width/3)), suffix, cacheStyle(cache), total.Value,
		barPart{m.Weighted.Value, "█", "#", liveStyle})
	parts := tokenParts(m)
	tokens := []string{"R " + metricText(m.Read, tokensText), "W " + metricText(m.Write, tokensText), "U " + metricText(m.Uncached, tokensText)}
	if m.UnsplitInput.Known {
		tokens = append(tokens, "N "+metricText(m.UnsplitInput, tokensText))
	}
	tokens = append(tokens, "O "+metricText(m.Output, tokensText))
	p.barRow("  ", 2, strings.Join(tokens, "  "), meta, partsTotal(parts), parts...)
}

func rankedChildren(rows []analysis.ChildSignal, mix map[session.ID]analysis.TokenMix) []analysis.ChildSignal {
	out := slices.Clone(rows)
	slices.SortFunc(out, func(a, b analysis.ChildSignal) int {
		return cmp.Or(compareUsage(mix[a.ID].Weighted, mix[b.ID].Weighted), cmp.Compare(a.Title, b.Title), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func analysisChildTitle(c analysis.ChildSignal) string {
	if c.Title == "" {
		return absentMeasure
	}
	return safeDiagnostic(c.Title)
}

func (p *analysisPainter) activeChildren(d *analysis.Dashboard, complete bool) {
	p.section("Active children by model", "モデル別の稼働中の子")
	active := make(map[string]int)
	total := 0
	hasUnknown := !complete
	for _, c := range d.Children {
		switch c.State {
		case "running":
			active[knownLabel(c.Model)]++
			total++
		case "":
			hasUnknown = true
		}
	}
	var models []string
	for model := range active {
		models = append(models, model)
	}
	slices.SortFunc(models, func(a, b string) int { return cmp.Or(cmp.Compare(active[b], active[a]), cmp.Compare(a, b)) })
	for _, model := range models {
		n := active[model]
		label := fmt.Sprintf("%d / %d", n, total)
		if hasUnknown {
			label = "~" + label
		}
		p.barRow(model, max(min(27, p.width/3), uniseg.StringWidth(model)+1), label, liveStyle, float64(total), barPart{float64(n), "█", "#", liveStyle})
	}
	if total == 0 {
		if hasUnknown {
			p.add(absentMeasure, meta)
		} else {
			p.add(p.tr("No active children", "稼働中の子はいません"), meta)
		}
	}
}

func recentHour(at, now time.Time) bool {
	return !at.IsZero() && !at.Before(now.Add(-time.Hour)) && !at.After(now)
}

func analysisTime(at, now time.Time) string {
	if at.IsZero() {
		return absentMeasure
	}
	if sameDisplayDate(at, now) {
		return displayTime(at, "15:04")
	}
	return displayTime(at, "01/02 15:04")
}

func (p *analysisPainter) allowanceGauge(w analysis.SubscriptionWindow, now time.Time, labelWidth int) {
	value := analysis.Metric{}
	if w.UsedPercent != nil && *w.UsedPercent >= 0 && *w.UsedPercent <= 100 {
		value = analysis.Metric{Known: true, Value: remainingPercent(*w.UsedPercent)}
	}
	resetLabel := p.tr("reset ", "更新 ")
	if w.State == "reset_since" {
		resetLabel = p.tr("reset passed ", "更新済み ")
	}
	st := allowanceStyle(value)
	window := subscriptionWindowLabel(w, Theme{ASCII: !p.ja})
	switch window {
	case "P":
		window = p.tr("primary", "主枠")
	case "S":
		window = p.tr("secondary", "副枠")
	}
	name := analysis.SubscriptionProviderName(w.Provider) + " " + window
	valueLabel := metricText(value, percentText) + p.severity(st)
	if w.Status != "" {
		if w.UsedPercent != nil && w.Status == "rejected" {
			valueLabel += "  " + p.tr("used ", "使用 ") + percentText(*w.UsedPercent)
		}
		valueLabel += "  " + w.Status
	}
	p.gauge(name, labelWidth, value, valueLabel+"  "+resetLabel+analysisTime(w.ResetsAt, now), st)
}

func (p *analysisPainter) subscriptions(r analysis.Report) {
	p.add(fmt.Sprintf(p.tr("Used %% · elapsed-time EMA %.2f/h · last %gh · dotted forecast", "使用率 · 時間加重 EMA %.2f/h · 直近%g時間 · 点線は予測"), analysis.EMAAlpha, analysis.BurnLookback.Hours()), meta)
	for _, w := range r.SubscriptionUsage {
		p.blank()
		p.allowanceGauge(w, r.At, min(23, p.width/3))
		f := r.ForecastAllowance(w)
		if w.UsedPercent != nil {
			p.chart(graph{height: 4, maximum: 100, format: percentText, markers: []time.Time{w.ResetsAt}, includeMarkers: true, series: []chartSeries{
				{points: f.History, st: liveStyle, mark: '#'},
				{points: f.Projection, st: warningStyle, mark: '.', dotted: true},
			}})
			prediction := absentMeasure
			forecastLabel := p.tr("At this rate ", "この速度では ")
			if !f.ExhaustedAt.IsZero() {
				forecastLabel = p.tr("At this rate, runs out at ", "この速度では ")
				prediction = analysisTime(f.ExhaustedAt, r.At) + p.tr("", " に枯渇")
				if !w.ResetsAt.IsZero() {
					switch {
					case f.ExhaustedAt.Before(w.ResetsAt):
						prediction += p.tr(" (before reset)", "（更新前）")
					case f.ExhaustedAt.Equal(w.ResetsAt):
						prediction += p.tr(" (at reset)", "（更新時）")
					default:
						prediction += p.tr(" (after reset)", "（更新後）")
					}
				}
			} else if f.Rate.Known && f.Rate.Value == 0 {
				prediction = p.tr("steady", "横ばい")
			}
			p.add(p.tr("Burn ", "消費 ")+metricText(f.Rate, func(v float64) string { return fmt.Sprintf("%.1f pp/h", v) })+"  "+forecastLabel+prediction, normal)
		}
		if !w.ObservedAt.IsZero() {
			text := p.tr("Observed ", "観測 ") + analysisTime(w.ObservedAt, r.At)
			if w.UsedPercent != nil && !w.PercentObservedAt.IsZero() {
				text += p.tr(" · percentage ", " · 使用率 ") + analysisTime(w.PercentObservedAt, r.At)
			}
			p.add(text, meta)
		}
	}
	if len(r.SubscriptionUsage) == 0 {
		p.add(absentMeasure, meta)
	}
}
