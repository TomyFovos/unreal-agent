package analysis

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type contextColumn struct{ key, name, unit string }

var boundaryColumns = []contextColumn{
	{"boundaries", "Boundaries", ""},
	{"reason_threshold", "Reason threshold", ""},
	{"reason_over_budget", "Reason over_budget", ""},
	{"reason_header_change", "Reason header_change", ""},
	{"reason_mid_turn", "Reason mid_turn", ""},
	{"reason_selection_change", "Reason selection_change", ""},
	{"reason_provider_overflow", "Reason provider_overflow", ""},
	{"reason_historical_context", "Reason historical_context", ""},
	{"boundary_before_tokens", "Input before rebuild", " tokens"},
	{"boundary_after_tokens", "Input after rebuild", " tokens"},
	{"window_header_tokens", "System and tool schemas after rebuild", " tokens"},
	{"required_minimum_over_target", "Required context exceeds rebuild target", ""},
	{"threshold_tokens", "W (configured)", " tokens"},
	{"l2_copy_tokens", "L2 copy", " tokens"},
	{"l2_target_tokens", "L2 target (configured)", " tokens"},
	{"tail_tokens", "L1 raw tail", " tokens"},
	{"tail_items", "L1 rendered items", ""},
	{"recent_events", "Recent events copied", ""},
	{"recent_event_limit", "Recent event count (configured)", ""},
	{"excerpt_chars", "Excerpt size (configured)", " chars"},
}

var fitColumns = []contextColumn{
	{"fit_excerpted", "Fit: excerpted", ""},
	{"fit_excerpt_chars", "Fitted head and tail allowance", " chars"},
	{"fit_required_input", "Required input before fitting", " tokens"},
	{"fit_required_tail", "Required L1 tail before fitting", " tokens"},
	{"fit_input_budget", "Input budget at fitting", " tokens"},
}

var intervalColumns = []contextColumn{
	{"boundary_interval_requests", "Requests since previous boundary", ""},
	{"boundary_interval_seconds", "Wall time since previous boundary", " s"},
	{"pending_approvals", "Pending approvals at boundary", ""},
	{"pending_user_questions", "Unanswered user questions at boundary", ""},
	{"pending_child_questions", "Unanswered child questions at boundary", ""},
}

var compactionColumns = []contextColumn{
	{"compactions", "Compaction events", ""},
	{"before_tokens", "Input before compaction", " tokens"},
	{"cli_post_tokens", "CLI post_tokens", " tokens"},
	{"post_request_input_tokens", "First post-compaction input", " tokens"},
	{"summary_output_tokens", "Summary output", " tokens"},
	{"generation_seconds", "Summary generation time", " s"},
}

var fallbackColumns = []contextColumn{
	{"before_tokens", "Input before rebuild", " tokens"},
	{"required_tokens", "Required three-layer input", " tokens"},
	{"header_tokens", "System and tool schemas", " tokens"},
	{"l2_tokens", "L2 copy", " tokens"},
	{"l2_target_tokens", "L2 target (configured)", " tokens"},
	{"tail_tokens", "Required L1 tail", " tokens"},
	{"input_budget_tokens", "Input budget", " tokens"},
}

var compactionFields = func() []string {
	var fields []string
	for _, c := range compactionColumns {
		fields = append(fields, c.key)
	}
	return fields
}()

var recordColumns = []contextColumn{
	{"record_show_calls", "record show calls", ""},
	{"record_show_bytes", "record show evidence", " bytes"},
	{"record_find_calls", "record find calls", ""},
	{"record_find_bytes", "record find excerpts", " bytes"},
	{"record_previous_tail_calls", "Calls re-reading previous L1 tail", ""},
	{"record_previous_tail_matches", "Matches in previous L1 tail", ""},
	{"record_previous_tail_percent", "Previous L1 tail re-read rate", "%"},
}

var questionColumns = []contextColumn{
	{"user_questions", "【要確認】 questions", ""},
	{"work_seconds", "Observed work time", " s"},
	{"questions_per_hour", "Questions per work hour", ""},
}

var reviewColumns = []contextColumn{
	{"normal", "Normally ended", ""},
	{"reviewed", "Explicitly reviewed", ""},
	{"accepted", "Accepted", ""},
	{"rejected", "Rejected", ""},
	{"unreviewed", "Unreviewed", ""},
	{"failed", "Failed", ""},
	{"canceled", "Canceled", ""},
	{"rejected_percent", "Rejected / reviewed", "%"},
}

type contextTable struct {
	title string
	rows  [][2]string
}

func (t *contextTable) add(numbers ContextNumbers, prefix string, columns []contextColumn) {
	for _, c := range columns {
		t.rows = append(t.rows, [2]string{c.name, contextNumberText(numbers[prefix+c.key], c.unit)})
	}
}

func contextNumberText(n ContextNumber, unit string) string {
	if n.Value == nil {
		return "unknown"
	}
	precision := 3
	if unit == " USD" {
		precision = 9
	}
	var value string
	if *n.Value == float64(int64(*n.Value)) && unit != " USD" {
		value = strconv.FormatFloat(*n.Value, 'f', 0, 64)
	} else {
		value = strconv.FormatFloat(*n.Value, 'f', precision, 64)
	}
	if n.Missing > 0 {
		return fmt.Sprintf("%s%s (%s, %d unknown)", value, unit, n.Source, n.Missing)
	}
	return value + unit + " (" + n.Source + ")"
}

func contextLabel(key ContextDimensions) string {
	return textOrUnknown(key.Strategy) + " / " + modelLabel(key.ModelKey) + " / fast " + textOrUnknown(key.Fast)
}

func textOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return viewer.SafeText(value)
}

func contextTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "unknown"
	}
	return timestamp(*at) + " (measured)"
}

func contextMeasurements(r Report) *ContextMeasurements {
	if r.Tree != nil && r.Tree.ContextMeasurements != nil {
		return r.Tree.ContextMeasurements
	}
	return r.ContextMeasurements
}

func contextTables(r *ContextMeasurements) []contextTable {
	if r == nil {
		return []contextTable{{title: "Context measurements unknown"}}
	}
	var tables []contextTable
	for _, s := range r.Sessions {
		tables = append(tables, scopeTables("Session "+string(s.Session), s.ContextScope)...)
		for _, w := range s.Windows {
			heading := "Session " + string(s.Session) + " · " + w.OpenedBy + " · " + contextTime(&w.At)
			for _, g := range w.Groups {
				tables = append(tables, groupTable(heading, g, true))
			}
		}
		for _, c := range s.Compactions {
			t := contextTable{title: "Session " + string(s.Session) + " · " + "Historical " + c.Engine + " compaction " + c.Outcome + " · " + contextTime(&c.At) + " · " + contextLabel(c.ContextDimensions)}
			t.add(c.Numbers, "", compactionColumns)
			tables = append(tables, t)
		}
		for _, f := range s.Fallbacks {
			t := contextTable{title: "Session " + string(s.Session) + " · Historical fallback: " + f.Reason + " · " + contextTime(&f.At) + " · " + contextLabel(f.ContextDimensions)}
			t.add(f.Numbers, "", fallbackColumns)
			tables = append(tables, t)
		}
	}
	return append(scopeTables("Session tree", r.Totals), tables...)
}

func scopeTables(title string, scope ContextScope) []contextTable {
	var tables []contextTable
	for _, group := range scope.Groups {
		tables = append(tables, groupTable(title, group, false))
	}
	for _, row := range scope.Questions {
		t := contextTable{title: title + " · User questions · " + textOrUnknown(row.Strategy)}
		t.add(row.Numbers, "", questionColumns)
		tables = append(tables, t)
	}
	for _, row := range scope.Reviews {
		t := contextTable{title: title + " · Child reviews · " + contextLabel(row.ContextDimensions) + " · child strategy " + textOrUnknown(row.ChildStrategy)}
		t.add(row.Numbers, "", reviewColumns)
		tables = append(tables, t)
	}
	for _, row := range scope.Subscription {
		tables = append(tables, contextTable{
			title: title + " · Subscription utilisation · " + string(row.Session) + " · " + contextTime(&row.At) + " · " + contextLabel(row.ContextDimensions),
			rows: [][2]string{
				{"Allowance window", textOrUnknown(row.Window)},
				{"Provider status", textOrUnknown(row.Status)},
				{"Utilisation", contextNumberText(row.UsedPercent, "%")},
				{"Window duration", contextNumberText(row.WindowMinutes, " min")},
				{"Reset time", contextTime(row.ResetsAt)},
			},
		})
	}
	if len(scope.Questions) == 0 {
		tables = append(tables, contextTable{title: title + " · User questions", rows: [][2]string{{"Questions per work hour", "unknown"}}})
	}
	if len(scope.Reviews) == 0 {
		tables = append(tables, contextTable{title: title + " · Child reviews", rows: [][2]string{{"Rejected / reviewed", "unknown"}}})
	}
	if len(scope.Subscription) == 0 {
		tables = append(tables, contextTable{title: title + " · Subscription utilisation", rows: [][2]string{{"Utilisation", "unknown"}}})
	}
	return tables
}

func groupTable(title string, g ContextGroup, window bool) contextTable {
	t := contextTable{title: title + " · " + contextLabel(g.ContextDimensions)}
	t.add(g.Numbers, "", groupColumns(g.Strategy, window))
	if _, present := g.Numbers["fit_excerpted"]; present {
		t.add(g.Numbers, "", fitColumns)
	}
	return t
}

var layeredGroupColumns = buildGroupColumns(contextengine.WindowThreeLayer, false)
var summaryGroupColumns = buildGroupColumns(historicalSummaryStrategy, false)
var unknownGroupColumns = buildGroupColumns("", false)
var layeredWindowColumns = buildGroupColumns(contextengine.WindowThreeLayer, true)
var summaryWindowColumns = buildGroupColumns(historicalSummaryStrategy, true)

func groupColumns(strategy string, window bool) []contextColumn {
	if window {
		if strategy == historicalSummaryStrategy {
			return summaryWindowColumns
		}
		return layeredWindowColumns
	}
	switch strategy {
	case contextengine.WindowThreeLayer:
		return layeredGroupColumns
	case historicalSummaryStrategy:
		return summaryGroupColumns
	default:
		return unknownGroupColumns
	}
}

func buildGroupColumns(strategy string, window bool) []contextColumn {
	var columns []contextColumn
	if strategy != historicalSummaryStrategy {
		columns = append(columns, boundaryColumns...)
	} else {
		columns = append(columns, boundaryColumns[0])
	}
	columns = append(columns, intervalColumns...)
	for _, phase := range []struct{ key, name string }{{"first", "First request"}, {"within", "Within window"}} {
		for _, c := range []contextColumn{
			{"requests", "requests", ""}, {"input", "total input", " tokens"},
			{"cache_read", "cache read", " tokens"}, {"cache_write", "cache write", " tokens"}, {"uncached", "uncached input", " tokens"},
			{"usd_uncached", "list price uncached", " USD"}, {"usd_read", "list price cache read", " USD"},
			{"usd_write", "list price cache write", " USD"}, {"usd_output", "list price output", " USD"}, {"usd_total", "list price total", " USD"},
		} {
			columns = append(columns, contextColumn{phase.key + "_" + c.key, phase.name + " " + c.name, c.unit})
		}
	}
	columns = append(columns, contextColumn{"within_hit_percent", "Within-window cache hit ratio", "%"})
	columns = append(columns, recordColumns...)
	for _, command := range stateCommands {
		columns = append(columns, contextColumn{"state_" + command, "state " + command, ""})
	}
	for _, kind := range machineKinds {
		columns = append(columns, contextColumn{"machine_" + string(kind), "Machine " + string(kind), ""})
	}
	columns = append(columns, questionColumns...)
	if !window && strategy != contextengine.WindowThreeLayer {
		for _, engine := range []string{"Unreal", "CLI"} {
			for _, c := range compactionColumns {
				columns = append(columns, contextColumn{engine + "_" + c.key, "Historical " + engine + " " + c.name, c.unit})
			}
		}
	}
	return columns
}

func contextNotes(r *ContextMeasurements) []string {
	if r == nil {
		return nil
	}
	lines := []string{
		"Tree totals include archived descendants. Boundary sizes and intervals are means of known samples in rollups.",
		"Cache hit ratio uses paired cache reads and total input. List-price equivalents in USD use " + r.PriceTable,
		"Work time is the union of recorded turn and tool intervals per Session. Tree work hours sum Session hours.",
		"Child reviews use the strategy at delegation and the child's model. Rejection rate uses normally ended children with explicit reviews.",
	}
	if r.Partial {
		lines = append(lines, "Partial context measurements. Unknown fields and samples are shown in the tables.")
	}
	return lines
}

func contextLines(r *ContextMeasurements) []string {
	out := contextNotes(r)
	for _, table := range contextTables(r) {
		out = append(out, "", table.title)
		for _, row := range table.rows {
			out = append(out, "  "+row[0]+"  "+row[1])
		}
	}
	return out
}

func writeContextMarkdown(b *strings.Builder, r *ContextMeasurements) {
	for _, note := range contextNotes(r) {
		b.WriteString(markdownText(note) + "\n\n")
	}
	cell := func(text string) string { return strings.ReplaceAll(markdownText(viewer.SafeText(text)), "|", "\\|") }
	for _, table := range contextTables(r) {
		b.WriteString("### " + cell(table.title) + "\n\n")
		if len(table.rows) > 0 {
			b.WriteString("| Metric | Value |\n| --- | --- |\n")
			for _, row := range table.rows {
				b.WriteString("| " + cell(row[0]) + " | " + cell(row[1]) + " |\n")
			}
			b.WriteByte('\n')
		}
	}
}
