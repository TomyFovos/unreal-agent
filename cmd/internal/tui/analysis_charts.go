package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
)

const absentMeasure = "—"

type analysisPainter struct {
	width int
	theme Theme
	ja    bool
	rows  []line
}

func (p *analysisPainter) tr(en, ja string) string {
	if p.ja {
		return ja
	}
	return en
}

func (p *analysisPainter) add(s string, st style) {
	p.rows = append(p.rows, textLine(SafeText(s), st).clip(p.width))
}

func (p *analysisPainter) section(en, ja string) {
	p.add(p.tr(en, ja), strong)
}

func (p *analysisPainter) blank() { p.rows = append(p.rows, nil) }

type barPart struct {
	value       float64
	mark, ascii string
	st          style
}

func (p *analysisPainter) stack(width int, total float64, parts ...barPart) line {
	if width <= 0 || total <= 0 {
		return nil
	}
	var result line
	var sum float64
	filled := 0
	for _, part := range parts {
		sum += max(0, part.value)
		next := min(width, int(math.Round(float64(width)*sum/total)))
		result = append(result, textLine(strings.Repeat(p.theme.symbol(part.mark, part.ascii), next-filled), part.st)...)
		filled = next
	}
	return result
}

func padCell(s string, n int, st style) line {
	if n <= 0 {
		return nil
	}
	result := textLine(SafeText(s), st).clip(n)
	return append(result, textLine(strings.Repeat(" ", n-result.width()), normal)...)
}

func (p *analysisPainter) titleCell(s string, width int, st style) line {
	label := textLine(SafeText(s), st)
	if label.width() <= width {
		return label
	}
	ellipsis := textLine(p.theme.symbol("…", "..."), st)
	if width < ellipsis.width() {
		return label.clip(width)
	}
	return joined(label.clip(width-ellipsis.width()), ellipsis)
}

func (p *analysisPainter) barRow(label string, labelWidth int, suffix string, st style, total float64, parts ...barPart) {
	suffixLine := textLine(suffix, st)
	labelWidth = max(0, min(labelWidth, p.width-suffixLine.width()-5))
	barWidth := max(0, p.width-labelWidth-suffixLine.width()-2)
	row := joined(padCell(label, labelWidth, normal), p.stack(barWidth, total, parts...))
	if suffix != "" && row.width() > 0 {
		row = append(row, textLine("  ", normal)...)
	}
	p.rows = append(p.rows, joined(row, suffixLine).clip(p.width))
}

func (p *analysisPainter) gauge(label string, labelWidth int, metric analysis.Metric, suffix string, st style) {
	if !metric.Known {
		p.barRow(label, labelWidth, suffix, meta, 0)
		return
	}
	p.barRow(label, labelWidth, suffix, st, 100,
		barPart{metric.Value, "█", "#", st}, barPart{100 - metric.Value, "░", "-", meta})
}

func metricText(m analysis.Metric, format func(float64) string) string {
	if !m.Known {
		return absentMeasure
	}
	prefix := ""
	if m.Unknown > 0 {
		prefix = "~"
	}
	return prefix + format(m.Value)
}

func tokensText(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1fG", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func percentText(v float64) string { return fmt.Sprintf("%.1f%%", v) }

func ratioMetric(n, d analysis.Metric) analysis.Metric {
	if !n.Known || !d.Known || d.Value <= 0 {
		return analysis.Metric{}
	}
	return analysis.Metric{Known: true, Value: 100 * n.Value / d.Value, Unknown: max(n.Unknown, d.Unknown)}
}

func cacheStyle(m analysis.Metric) style {
	switch {
	case !m.Known:
		return meta
	case m.Value < 75:
		return failureStyle
	case m.Value < 90:
		return warningStyle
	default:
		return successStyle
	}
}

func allowanceStyle(m analysis.Metric) style {
	switch {
	case !m.Known:
		return meta
	case m.Value <= 10:
		return failureStyle
	case m.Value <= 20:
		return warningStyle
	default:
		return successStyle
	}
}

func rateStyle(m analysis.Metric) style {
	switch {
	case !m.Known:
		return meta
	case m.Value >= 15:
		return failureStyle
	case m.Value >= 5:
		return warningStyle
	default:
		return successStyle
	}
}

func staleStyle(age time.Duration) style {
	switch {
	case age > 45*time.Minute:
		return failureStyle
	case age > 15*time.Minute:
		return warningStyle
	default:
		return successStyle
	}
}

func (p *analysisPainter) severity(st style) string {
	switch st {
	case warningStyle:
		return " !"
	case failureStyle:
		return " " + p.theme.symbol("×", "x")
	default:
		return ""
	}
}

type chartSeries struct {
	points []analysis.Sample
	st     style
	mark   rune
	dotted bool
}

type graph struct {
	series         []chartSeries
	markers        []time.Time
	includeMarkers bool
	limit          analysis.Metric
	maximum        float64
	height         int
	format         func(float64) string
}

type pixel struct {
	dots  rune
	style style
	mark  rune
}

// chart rasterizes at two by four dots per terminal cell. The ASCII grid
// uses the same time scale and leaves visible gaps in the forecast.
func (p *analysisPainter) chart(g graph) {
	var start, end time.Time
	high := max(g.maximum, g.limit.Value)
	known := false
	include := func(at time.Time) {
		if at.IsZero() {
			return
		}
		if start.IsZero() || at.Before(start) {
			start = at
		}
		if at.After(end) {
			end = at
		}
	}
	for _, series := range g.series {
		for _, sample := range series.points {
			include(sample.At)
			if !sample.At.IsZero() && sample.Known {
				known = true
				high = max(high, sample.Value)
			}
		}
	}
	if g.includeMarkers {
		for _, at := range g.markers {
			include(at)
		}
	}
	if !known || start.IsZero() || p.width < 16 {
		p.add(absentMeasure, meta)
		return
	}
	if end.Equal(start) {
		// A single observation is placed at the right edge and has no slope.
		start = end.Add(-time.Minute)
	}
	high = max(1, high)
	gutter := max(4, len(g.format(high)))
	cols := p.width - gutter - 2
	if cols < 11 {
		p.add(absentMeasure, meta)
		return
	}
	w, h := 2, 4
	if p.theme.ASCII {
		w, h = 1, 1
	}
	nx := cols * w
	grid := make([]pixel, cols*g.height)
	xOf := func(at time.Time) int {
		return int(math.Round(float64(nx-1) * float64(at.Sub(start)) / float64(end.Sub(start))))
	}
	yOf := func(v float64) int {
		return h/2 + int(math.Round(float64((g.height-1)*h)*(1-min(1, max(0, v/high)))))
	}
	for _, series := range g.series {
		plot := func(x, y int, force bool) {
			if series.dotted && !force && (series.mark == '.' && x/w%3 != 0 || series.mark == '=' && x/w%3 == 2) {
				return
			}
			cell := &grid[(y/h)*cols+x/w]
			cell.style, cell.mark = series.st, series.mark
			if !p.theme.ASCII {
				bit := [2][4]rune{{1, 2, 4, 64}, {8, 16, 32, 128}}[x%w][y%h]
				cell.dots |= bit
			}
		}
		// A column shows its peak observation. This keeps short input spikes
		// and avoids drawing every transition within a single terminal cell.
		bins := make([]analysis.Sample, cols)
		for _, s := range series.points {
			if s.At.IsZero() {
				continue
			}
			i := xOf(s.At) / w
			if bins[i].At.IsZero() || s.Known && (!bins[i].Known || s.Value >= bins[i].Value) {
				bins[i] = s
			}
		}
		lastX, lastY, connected := 0, 0, false
		for _, s := range bins {
			if s.At.IsZero() {
				continue
			}
			if !s.Known {
				connected = false
				continue
			}
			x, y := xOf(s.At), yOf(s.Value)
			if connected && x >= lastX {
				dx, dy := x-lastX, y-lastY
				steps := max(dx, int(math.Abs(float64(dy))))
				for i := 1; i < steps; i++ {
					frac := float64(i) / float64(steps)
					plot(lastX+int(math.Round(float64(dx)*frac)), lastY+int(math.Round(float64(dy)*frac)), false)
				}
			}
			plot(x, y, true)
			lastX, lastY, connected = x, y, true
		}
	}

	guideY := yOf(g.limit.Value)
	for row := range g.height {
		label := ""
		switch row {
		case 0:
			label = g.format(high)
		case g.height - 1:
			label = g.format(0)
		default:
			if row == g.height/2 {
				label = g.format(high * float64(g.height-1-row) / float64(g.height-1))
			}
		}
		var result line
		guide := g.limit.Known && guideY/h == row
		ruler := p.theme.symbol("│", "|")
		if guide {
			ruler, label = "W", g.format(g.limit.Value)
		}
		result = textLine(fmt.Sprintf("%*s %s", gutter, label, ruler), meta)
		for x := range cols {
			cell := grid[row*cols+x]
			mark, color := " ", cell.style
			switch {
			case cell.dots != 0:
				mark = string(0x2800 + cell.dots)
			case cell.mark != 0:
				mark = string(cell.mark)
			case guide && x%2 == 0:
				mark, color = p.theme.symbol("─", "-"), meta
				if !p.theme.ASCII {
					mark = string(0x2800 + [4]rune{9, 18, 36, 192}[guideY%h])
				}
			}
			result = append(result, glyph{text: mark, width: 1, style: color})
		}
		p.rows = append(p.rows, result)
	}
	marks := make([]string, cols)
	for i := range marks {
		marks[i] = p.theme.symbol("─", "-")
	}
	for _, at := range g.markers {
		if !at.IsZero() && !at.Before(start) && !at.After(end) {
			marks[xOf(at)/w] = p.theme.symbol("┬", "+")
		}
	}
	p.add(strings.Repeat(" ", gutter+1)+p.theme.symbol("└", "+")+strings.Join(marks, ""), meta)
	format := "15:04"
	if !sameDisplayDate(start, end) && cols >= 32 {
		format = "01/02 15:04"
	}
	left, right := displayTime(start, format), displayTime(end, format)
	if len(right) > cols {
		p.add(right, meta)
		return
	}
	labels := []byte(strings.Repeat(" ", cols))
	if len(left)+len(right)+2 <= cols {
		copy(labels, left)
	}
	copy(labels[cols-len(right):], right)
	if cols > len(left)+len(right)+len(left)+4 {
		middle := displayTime(start.Add(end.Sub(start)/2), format)
		copy(labels[(cols-len(middle))/2:], middle)
	}
	p.add(strings.Repeat(" ", gutter+2)+string(labels), meta)
}

func datedSamples(points []analysis.Sample) []analysis.Sample {
	ordered := slices.Clone(points)
	slices.SortStableFunc(ordered, func(a, b analysis.Sample) int { return a.At.Compare(b.At) })
	return ordered
}
