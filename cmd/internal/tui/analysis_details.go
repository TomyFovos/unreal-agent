package tui

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func (p *analysisPainter) contextView(r analysis.Report, c analysis.ContextSignal) {
	measurements := r.ContextMeasurements
	if r.Tree != nil && r.Tree.ContextMeasurements != nil {
		measurements = r.Tree.ContextMeasurements
	}
	p.contextPolicies(measurements)
	p.section("Request input · tokens · W rebuild threshold", "リクエスト入力 · トークン · W 再構築しきい値")
	markers := make([]time.Time, 0, len(c.Boundaries))
	for _, b := range c.Boundaries {
		markers = append(markers, b.At)
	}
	p.chart(graph{height: 5, format: tokensText, limit: c.Threshold, markers: markers, includeMarkers: true,
		series: []chartSeries{{points: datedSamples(c.Requests), st: youStyle, mark: '#'}}})
	p.add("W "+metricText(c.Threshold, tokensText)+"  "+p.theme.symbol("┬", "+")+" "+p.tr("boundary", "再構築境界"), meta)
	p.blank()
	p.section("Boundary causes · share of rebuilds", "再構築の原因 · 境界全体に占める割合")
	causes := make(map[string]int)
	for _, b := range c.Boundaries {
		for i, cause := range b.Causes {
			if cause != "" && !slices.Contains(b.Causes[:i], cause) {
				causes[cause]++
			}
		}
	}
	for _, cause := range slices.Sorted(maps.Keys(causes)) {
		n := causes[cause]
		suffix := fmt.Sprintf("%d / %d", n, len(c.Boundaries))
		p.barRow(cause, 20, suffix, normal, float64(len(c.Boundaries)), barPart{float64(n), "█", "#", liveStyle})
	}
	if len(causes) == 0 {
		p.add(absentMeasure, meta)
	}
	p.blank()
	p.section("Cache read share · first after boundary vs steady", "キャッシュ読取率 · 境界直後と定常時")
	p.cacheGauge(p.tr("First", "直後"), 10, c.FirstRead)
	p.cacheGauge(p.tr("Steady", "定常"), 10, c.SteadyRead)
	p.section("Latest window · recorded L1 + L2 tokens", "最新ウィンドウ · 記録済みの L1 + L2 トークン")
	total := c.L1.Value + c.L2.Value
	suffix := "L1 " + metricText(c.L1, tokensText) + "  L2 " + metricText(c.L2, tokensText) + "  L3 " + absentMeasure
	p.barRow("", 0, suffix, meta, total, barPart{c.L1.Value, "█", "#", youStyle}, barPart{c.L2.Value, "▓", "=", liveStyle})
	if r.ContextPackage != nil && r.ContextPackage.Excluded > 0 {
		d := r.ContextPackage
		if d.CanonicalUnits > 0 {
			p.add(fmt.Sprintf(p.tr("Security exclusions %d / %d", "セキュリティ除外 %d / %d"), d.Excluded, d.CanonicalUnits), warningStyle)
		}
	}
	if c.Legacy > 0 {
		p.add(fmt.Sprintf(p.tr("legacy summary-strategy records: %d", "旧要約方式の記録 %d 件"), c.Legacy), meta)
	}
}

func (p *analysisPainter) contextPolicies(c *analysis.ContextMeasurements) {
	if c == nil {
		return
	}
	wrap := func(text string) {
		p.rows = append(p.rows, wrapLine(textLine(SafeText(text), meta), p.width, false, 2)...)
	}
	if len(c.ActivePolicies) > 0 {
		p.section("Latest applied policy by route", "ルート別の適用済みポリシー")
		for _, route := range c.ActivePolicies {
			name := route.Provider
			if name == "" {
				name = p.tr("Default", "既定")
			}
			policy := route.Policy
			wrap(name + "  " + windowPolicyText(policy))
			wrap(fmt.Sprintf(p.tr("  Recent %d · excerpt %d chars · rewarm %dm · input cap %d · fallback %d · reserves %d + %d",
				"  直近 %d · 抜粋 %d 字 · 再加温 %d 分 · 入力上限 %d · 既定窓 %d · 予約 %d + %d"),
				policy.RecentEventCount, policy.ToolOutputExcerptChars, policy.CacheRewarmMinutes,
				policy.InputBudget, policy.FallbackWindow, policy.ResponseReserve, policy.ProtocolReserve))
		}
		p.blank()
	}
	showedPeriods := false
	for _, s := range c.Sessions {
		if len(s.PolicyPeriods) == 0 {
			continue
		}
		if !showedPeriods {
			p.section("Rebuilds by policy period", "ポリシー期間別の再構築")
			showedPeriods = true
		}
		p.add(p.tr("Session ", "セッション ")+string(s.Session), strong)
		for _, period := range s.PolicyPeriods {
			policy := p.tr("unknown policy", "ポリシー不明")
			if period.Policy != nil {
				policy = windowPolicyText(*period.Policy)
			}
			dates := absentMeasure
			if period.From != nil {
				dates = displayTime(*period.From, "01/02 15:04:05")
			}
			if period.Until != nil {
				dates += " → " + displayTime(*period.Until, "01/02 15:04:05")
			}
			wrap(knownLabel(period.Provider) + "  " + dates + "  " + policy)
			metric := func(key string, format func(float64) string) string {
				n := period.Numbers[key]
				if n.Value == nil {
					return absentMeasure
				}
				return metricText(analysis.Metric{Value: *n.Value, Known: true, Unknown: n.Missing}, format)
			}
			wrap(fmt.Sprintf(p.tr("  Rebuilds %s · mean after %s tokens · first cache read %s · L3 show %s / find %s",
				"  再構築 %s 回 · 直後平均 %s トークン · 初回読取率 %s · L3 show %s / find %s"),
				metric("rebuilds", tokensText), metric("boundary_after_tokens", tokensText),
				metric("first_cache_read_share", percentText), metric("record_show_calls", tokensText), metric("record_find_calls", tokensText)))
		}
	}
	if showedPeriods {
		p.blank()
	}
}

func windowPolicyText(policy sessionstore.WindowPolicy) string {
	return fmt.Sprintf("W %d · ×%g · L2 %d", policy.WindowThresholdTokens, policy.PostRebuildRatio, policy.L2TargetTokens)
}

func (p *analysisPainter) agents(now time.Time, d *analysis.Dashboard) {
	_, _, tokens, total := usageGroups(d)
	ranked := rankedChildren(d.Children, tokens)
	if len(ranked) == 0 {
		p.add(absentMeasure, meta)
		return
	}
	durationOf := func(c analysis.ChildSignal) analysis.Metric {
		end := c.End
		if c.State == "running" {
			end = now
		}
		if c.Start.IsZero() || end.IsZero() || end.Before(c.Start) {
			return analysis.Metric{}
		}
		return analysis.Metric{Known: true, Value: end.Sub(c.Start).Seconds()}
	}
	longest := 0.0
	for _, c := range ranked {
		longest = max(longest, durationOf(c).Value)
	}
	wide := p.width >= 145
	roleWidth, modelWidth := 11, 19
	modelLabel := func(c analysis.ChildSignal) string {
		if c.Effort == "" {
			return knownLabel(c.Model)
		}
		return knownLabel(c.Model) + p.theme.symbol("·", "/") + c.Effort
	}
	for _, child := range ranked {
		modelWidth = max(modelWidth, uniseg.StringWidth(modelLabel(child))+1)
	}
	modelWidth = min(modelWidth, max(10, p.width-49))
	titleWidth := min(34, max(10, p.width-modelWidth-roleWidth-26))
	if wide {
		titleWidth = min(titleWidth, max(10, p.width-modelWidth-78))
	}
	if p.width < 60 {
		titleWidth, roleWidth, modelWidth = max(9, p.width-24), 0, 0
	}
	rowLabel := func(title, role, model, state string, st style) line {
		label := p.titleCell(title, titleWidth, strong)
		return joined(label, padCell("", titleWidth-label.width()+1, normal), padCell(role, roleWidth, normal), padCell(model, modelWidth, normal), padCell(state, 11, st))
	}
	header := rowLabel(p.tr("Title", "タイトル"), p.tr("Role", "役割"), p.tr("Model·effort", "モデル・effort"), p.tr("State", "状態"), strong)
	if wide {
		header = append(header, padCell(p.tr("Result", "結果"), 13, strong)...)
		header = append(header, padCell(p.tr("Time / max", "時間 / 最大"), 19, strong)...)
		header = append(header, padCell(p.tr("Weighted / total", "加重 / 全体"), 21, strong)...)
		header = append(header, padCell(p.tr("Cache", "読取率"), 9, strong)...)
		header = append(header, textLine(p.tr("Since last", "最終活動から"), strong)...)
	} else {
		header = append(header, textLine(p.tr("Result", "結果"), strong)...)
	}
	p.rows = append(p.rows, header.clip(p.width))
	for _, child := range ranked {
		state, result := p.childStatus(child)
		stateColor := normal
		if child.Result == "failed" {
			stateColor = failureStyle
		}
		model := modelLabel(child)
		if uniseg.StringWidth(model) >= modelWidth {
			model = knownLabel(child.Model)
		}
		row := rowLabel(analysisChildTitle(child), knownLabel(child.Role), model, state, stateColor)
		row = append(row, padCell(result, 13, stateColor)...)
		m := tokens[child.ID]
		cache, share := m.Cache.Percent(), ratioMetric(m.Weighted, total.Weighted)
		d := durationOf(child)
		took := metricText(d, secondsText)
		amount := metricText(m.Weighted, tokensText)
		cacheLabel := metricText(cache, percentText) + p.severity(cacheStyle(cache))
		age, ageStyle := absentMeasure, meta
		if child.State == "running" && !child.LastActivity.IsZero() && !child.LastActivity.After(now) {
			gap := now.Sub(child.LastActivity)
			ageStyle = staleStyle(gap)
			age = duration(gap) + p.severity(ageStyle)
		}
		var detailed line
		component := func(width int, value, scale float64, label string, st style) {
			bar := p.stack(width, scale, barPart{value, "█", "#", st})
			detailed = append(detailed, bar...)
			detailed = append(detailed, textLine(strings.Repeat(" ", width-bar.width()), normal)...)
			detailed = append(detailed, textLine(" "+label+"  ", st)...)
		}
		if wide {
			component(6, d.Value, longest, took, youStyle)
			detailed = append(detailed, padCell("", max(0, 19-detailed.width()), normal)...)
			before := detailed.width()
			component(6, m.Weighted.Value, total.Weighted.Value, amount+" "+metricText(share, percentText), liveStyle)
			detailed = append(detailed, padCell("", max(0, 21-(detailed.width()-before)), normal)...)
			detailed = append(detailed, padCell(cacheLabel, 9, cacheStyle(cache))...)
			p.rows = append(p.rows, joined(row, detailed, textLine(age, ageStyle)).clip(p.width))
		} else {
			p.rows = append(p.rows, row.clip(p.width))
			if p.width >= 54 {
				component(5, d.Value, longest, took, youStyle)
			}
			component(5, m.Weighted.Value, total.Weighted.Value, amount+" "+metricText(share, percentText), liveStyle)
			detailed = append(detailed, textLine(p.tr("Cache ", "読取 ")+cacheLabel+"  ", cacheStyle(cache))...)
			if child.State == "running" {
				detailed = append(detailed, textLine(p.tr("Last ", "活動 ")+age, ageStyle)...)
			}
			p.rows = append(p.rows, joined(textLine("  ", normal), detailed).clip(p.width))
		}
	}
	if !wide {
		p.add(p.tr("Bars · time vs longest, weighted tokens vs Session", "棒 · 時間は最長比、加重トークンは全体比"), meta)
	}
}

func knownLabel(s string) string {
	if s == "" || s == "unknown" {
		return absentMeasure
	}
	return s
}

func (p *analysisPainter) childStatus(c analysis.ChildSignal) (state, result string) {
	switch c.State {
	case "running":
		state = p.tr("Running", "稼働中")
	case "stopped":
		state = p.tr("Stopped", "停止")
	case "ended":
		state = p.tr("Ended", "終了")
	default:
		state = knownLabel(c.State)
	}
	switch c.Result {
	case "completed":
		result = p.tr("Completed", "完了")
	case "failed":
		result = p.tr("Failed", "失敗")
	case "canceled":
		result = p.tr("Canceled", "取消")
	default:
		result = knownLabel(c.Result)
	}
	return
}

func secondsText(v float64) string {
	if v > 0 && v < 1 {
		return fmt.Sprintf("%.2fs", v)
	}
	return duration(time.Duration(v * float64(time.Second)))
}

type toolGroup struct {
	name                                        string
	elapsed                                     analysis.Metric
	failed, resolved, unfinished, unknownResult int
}

func failureRate(failed, resolved int) analysis.Metric {
	if resolved <= 0 {
		return analysis.Metric{}
	}
	return analysis.Metric{Known: true, Value: 100 * float64(failed) / float64(resolved)}
}

func (p *analysisPainter) tools(d *analysis.Dashboard, legacy []analysis.Tool) {
	totals := make(map[string]*toolGroup)
	var slow []analysis.ToolSample
	var total float64
	for _, call := range d.Tools {
		group := totals[call.Name]
		if group == nil {
			group = &toolGroup{name: knownLabel(call.Name)}
			totals[call.Name] = group
		}
		m := call.Duration()
		if m.Known {
			group.elapsed.Known = true
			group.elapsed.Value += m.Value
			total += m.Value
			slow = append(slow, call)
		} else {
			group.elapsed.Unknown++
		}
		switch call.Result {
		case "failed":
			group.failed++
			group.resolved++
		case "completed":
			group.resolved++
		case "unknown":
			group.unknownResult++
		case "":
			group.unfinished++
		}
	}
	if len(d.Tools) == 0 {
		for _, tool := range legacy {
			elapsed := analysis.Metric{Value: tool.Elapsed.Value.Seconds(), Known: tool.Elapsed.Known}
			if tool.ElapsedPartial {
				elapsed.Unknown++
			}
			totals[tool.Name] = &toolGroup{name: tool.Name, elapsed: elapsed, failed: tool.Failed, resolved: tool.Completed + tool.Failed, unfinished: tool.Unresolved}
			total += elapsed.Value
		}
	}
	groups := make([]*toolGroup, 0, len(totals))
	for _, group := range totals {
		groups = append(groups, group)
	}
	slices.SortFunc(groups, func(a, b *toolGroup) int {
		return cmp.Or(compareUsage(a.elapsed, b.elapsed), cmp.Compare(a.name, b.name))
	})
	p.section("Tool time · share of measured call time", "ツール時間 · 計測済みの呼出時間に占める割合")
	for _, group := range groups {
		label := min(28, p.width/3)
		share := ratioMetric(group.elapsed, analysis.Metric{Known: total > 0, Value: total})
		p.barRow(group.name, label, metricText(group.elapsed, secondsText)+"  "+metricText(share, percentText), normal, total,
			barPart{group.elapsed.Value, "█", "#", youStyle})
		rate := failureRate(group.failed, group.resolved)
		rate.Unknown = group.unfinished + group.unknownResult
		st := rateStyle(rate)
		suffix := metricText(rate, percentText) + p.severity(st)
		if group.unknownResult > 0 {
			suffix += fmt.Sprintf(p.tr("  %d result unknown", "  結果不明 %d"), group.unknownResult)
		}
		if group.unfinished > 0 {
			suffix += fmt.Sprintf(p.tr("  %d incomplete", "  未完了 %d"), group.unfinished)
		}
		p.gauge(p.tr("  Failure rate", "  失敗率"), label, rate, suffix, st)
	}
	if len(groups) == 0 {
		p.add(absentMeasure, meta)
	}
	p.blank()
	p.section("Slowest calls · relative to the longest", "最も遅い呼出 · 最長の呼出との比較")
	if len(slow) == 0 {
		p.add(absentMeasure, meta)
		return
	}
	slices.SortFunc(slow, func(a, b analysis.ToolSample) int {
		return cmp.Or(cmp.Compare(b.Duration().Value, a.Duration().Value), b.End.Compare(a.End), cmp.Compare(a.Name, b.Name))
	})
	maximum := slow[0].Duration().Value
	for _, call := range slow[:min(5, len(slow))] {
		d := call.Duration()
		p.barRow(call.Name, min(28, p.width/3), metricText(d, secondsText), normal, maximum, barPart{d.Value, "█", "#", youStyle})
	}
}

func eventKind(kind string) string {
	switch kind {
	case "provider retry":
		return "provider"
	case "child failure":
		return "child"
	default:
		return kind
	}
}

func (p *analysisPainter) failureBars(r analysis.Report, d *analysis.Dashboard, complete, all bool) {
	p.section("Failures · last hour / Session · hourly rate", "失敗 · 直近1時間 / 全体 · 1時間の失敗率")
	kinds := []string{"tool", "child", "provider"}
	labels := []string{p.tr("Tools", "ツール"), p.tr("Children", "子"), p.tr("Provider", "プロバイダ")}
	resolved := make(map[string]int)
	for _, t := range d.Tools {
		if (t.Result == "failed" || t.Result == "completed") && recentHour(t.End, r.At) {
			resolved["tool"]++
		}
	}
	for _, c := range d.Children {
		if (c.Result == "failed" || c.Result == "completed") && recentHour(c.End, r.At) {
			resolved["child"]++
		}
	}
	if all {
		kinds = append(kinds, "crash", "connection", "session")
		labels = append(labels, p.tr("Crashes", "クラッシュ"), p.tr("Connection", "接続"), p.tr("Session", "セッション"))
	}
	for i, kind := range kinds {
		last, total, undated := 0, 0, false
		for _, e := range d.Failures {
			if eventKind(e.Kind) != kind {
				continue
			}
			total++
			undated = undated || e.At.IsZero()
			if recentHour(e.At, r.At) {
				last++
			}
		}
		lastText, totalText := fmt.Sprint(last), fmt.Sprint(total)
		if !complete {
			if total == 0 {
				lastText, totalText = absentMeasure, absentMeasure
			} else {
				lastText, totalText = "~"+lastText, "~"+totalText
			}
		}
		if undated && complete {
			lastText = "~" + lastText
		}
		if kind == "provider" {
			resolved[kind] = last
			for _, at := range d.ProviderOK {
				if recentHour(at, r.At) {
					resolved[kind]++
				}
			}
		}
		rate := failureRate(last, resolved[kind])
		if !complete || undated {
			rate.Unknown++
		}
		st := rateStyle(rate)
		suffix := lastText + " / " + totalText
		if i < 3 {
			suffix += "  " + metricText(rate, percentText) + p.severity(st)
		}
		p.barRow(labels[i], 15, suffix, st, float64(total), barPart{float64(last), "█", "#", st}, barPart{float64(total - last), "░", "-", meta})
	}
}

func (p *analysisPainter) errors(r analysis.Report, d *analysis.Dashboard, complete bool) {
	p.failureBars(r, d, complete, true)
	p.blank()
	p.section("Recent failures", "最近の失敗")
	events := slices.Clone(d.Failures)
	slices.SortFunc(events, func(a, b analysis.Event) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})
	if len(events) == 0 {
		p.add(absentMeasure, meta)
	}
	for _, e := range events {
		p.add(analysisTime(e.At, r.At)+"  "+e.Kind+"  "+safeDiagnostic(knownLabel(e.Name)), failureStyle)
	}
}

func (p *analysisPainter) turns(r analysis.Report) {
	p.section("Largest input first", "入力の多い順")
	p.add(p.tr("R cached input  U other input  O output · relative to largest turn", "R キャッシュ読取  U その他の入力  O 出力 · 最大ターン比"), meta)
	type measuredTurn struct {
		analysis.Turn
		mix analysis.TokenMix
	}
	turns := make([]measuredTurn, 0, len(r.Turns))
	var scale float64
	for _, turn := range r.Turns {
		var mix analysis.TokenMix
		mix.Add(turn.Usage)
		scale = max(scale, mix.Input.Value+mix.Output.Value)
		turns = append(turns, measuredTurn{turn, mix})
	}
	if len(r.Turns) == 0 {
		p.add(absentMeasure, meta)
	}
	slices.SortStableFunc(turns, func(a, b measuredTurn) int { return compareUsage(a.mix.Input, b.mix.Input) })
	for _, turn := range turns {
		m := turn.mix
		other := analysis.Metric{}
		if m.Input.Known && m.Read.Known && m.Input.Value >= m.Read.Value {
			other = analysis.Metric{Known: true, Value: m.Input.Value - m.Read.Value}
		}
		model := turn.ObservedModel
		if model == "" && turn.Selection != nil {
			model = turn.Selection.Name
		}
		suffix := "R " + metricText(m.Read, tokensText) + "  U " + metricText(other, tokensText) + "  O " + metricText(m.Output, tokensText)
		label := fmt.Sprintf("#%d", turn.Number)
		if p.width >= 65 && model != "" {
			label += " " + model
		}
		p.barRow(label, max(min(25, p.width/3), uniseg.StringWidth(label)+1), suffix, meta, scale, barPart{m.Read.Value, "█", "#", successStyle},
			barPart{other.Value, "▒", "=", youStyle}, barPart{m.Output.Value, "░", "-", liveStyle})
	}
}

func (p *analysisPainter) timeView(r analysis.Report, d *analysis.Dashboard, complete bool) {
	start, end := d.Wall.Start, d.Wall.End
	if end.IsZero() {
		end = d.Wall.LastRecord
	}
	if d.State == "running" || d.State == "usage_limit_wait" {
		end = r.At
	}
	spans := d.Wall.Partition(end, complete)
	if len(spans) == 0 {
		p.add(absentMeasure, meta)
		return
	}
	totals := analysis.SpanTotals(spans, start, end)
	parts := [analysis.TimeKinds]barPart{
		{0, "░", "-", meta}, {0, "·", ".", meta}, {0, "█", "#", liveStyle},
		{0, "▓", "=", youStyle}, {0, "▒", "+", successStyle}, {0, "▰", "~", warningStyle}, {0, "▮", "|", failureStyle},
	}
	labels := [analysis.TimeKinds]string{p.tr("Idle", "アイドル"), p.tr("Unattributed", "内訳不明"), p.tr("Model generation", "モデル生成"),
		p.tr("Tools", "ツール"), p.tr("Waiting for children", "子の完了待ち"), p.tr("Usage limit", "利用制限待ち"), p.tr("Approval", "承認待ち")}
	stack := func(times [analysis.TimeKinds]time.Duration) []barPart {
		values := parts
		for i, t := range times {
			values[i].value = t.Seconds()
		}
		return values[:]
	}
	total := end.Sub(start).Seconds()
	p.barRow(p.tr("Session", "全体"), 12, secondsText(total)+"  100%", normal, total, stack(totals)...)
	for i, part := range parts {
		share := absentMeasure
		if totals[i] > 0 {
			share = secondsText(totals[i].Seconds()) + "  " + percentText(100*totals[i].Seconds()/total)
		}
		p.add(p.theme.symbol(part.mark, part.ascii)+" "+labels[i]+"  "+share, part.st)
	}
	p.blank()
	p.section("Per hour · identical consecutive hours grouped", "時間別 · 同じ内訳の連続時間はまとめて表示")
	for at, cursor := end.Add(-time.Nanosecond).Truncate(time.Hour), len(spans)-1; at.Add(time.Hour).After(start); {
		through := at.Add(time.Hour)
		for cursor > 0 && !spans[cursor].Start.Before(through) {
			cursor--
		}
		first, from := at, cursor
		latest := spans[cursor]
		if !latest.Start.After(at) && !latest.End.Before(through) {
			first = latest.Start.Truncate(time.Hour)
			if first.Before(latest.Start) {
				first = first.Add(time.Hour)
			}
		} else {
			for from > 0 && spans[from-1].End.After(at) {
				from--
			}
		}
		times := analysis.SpanTotals(spans[from:cursor+1], at, through)
		values := stack(times)
		label := displayTime(at, "01/02 15:04")
		suffix := secondsText(partsTotal(values)) + " / 1h"
		if first.Before(at) {
			label = displayTime(first, "01/02 15:04") + p.theme.symbol("–", "-") + displayTime(through, "01/02 15:04")
			suffix += fmt.Sprintf(" ×%gh", through.Sub(first).Hours())
		}
		p.barRow(label, uniseg.StringWidth(label)+2, suffix, normal, time.Hour.Seconds(), values...)
		at, cursor = first.Add(-time.Hour), from
	}
}
