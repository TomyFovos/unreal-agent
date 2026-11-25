package tui

import (
	"context"
	"os"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

type analysisTarget struct {
	view    string
	width   int
	ja, raw bool
}

type analysisScreen struct {
	ctx     context.Context
	cancel  context.CancelFunc
	picker  *Picker
	view    string
	offset  int
	ja, raw bool

	report       *analysis.Report
	rendered     analysisTarget
	rows         []line
	problem      string
	loading      bool
	exportFormat string

	version, viewerVersion uint64
	nextRead, refreshAt    time.Time
}

type analysisUpdate struct {
	screen *analysisScreen
	report *analysis.Report
	target analysisTarget
	rows   []line
	err    error
}

func openAnalysis(ctx context.Context) *analysisScreen {
	ctx, cancel := context.WithCancel(ctx)
	return &analysisScreen{ctx: ctx, cancel: cancel, picker: analyzePicker(""), ja: analysisJapanese(os.Getenv)}
}

func (p *analysisScreen) request(m *Model, inspect analysis.InspectFunc, readUsage func(context.Context, time.Duration) (subagent.UsageReport, error), s Snapshot, v viewer.PanelSnapshot, theme Theme, now time.Time, width int, updates chan<- analysisUpdate) {
	if p.loading {
		return
	}
	target := analysisTarget{view: p.view, width: width, ja: p.ja, raw: p.raw}
	expired := !p.refreshAt.IsZero() && !now.Before(p.refreshAt)
	read := !now.Before(p.nextRead) && (p.report == nil || p.version != s.Version || p.viewerVersion != v.Version || expired)
	render := p.view != "" && p.report != nil && p.rendered != target
	if !read && !render {
		return
	}
	p.loading = true
	report := p.report
	ctx := p.ctx
	if read {
		p.version, p.viewerVersion = s.Version, v.Version
		p.nextRead = now.Add(time.Second)
	}
	go func() {
		result := analysisUpdate{screen: p, target: target}
		if read {
			r, err := m.readAnalysisTree(ctx, inspect, now, v.Rows, v.SubscriptionUsage)
			result.err = err
			if err == nil {
				if readUsage != nil {
					if ledger, err := readUsage(ctx, time.Hour); err == nil {
						r.Ledger = &ledger
					}
				}
				report, result.report = &r, &r
			}
		}
		if target.view != "" && report != nil {
			result.rows = analysisRows(*report, target, theme)
		}
		select {
		case updates <- result:
		case <-ctx.Done():
		}
	}()
}

func (p *analysisScreen) accept(result analysisUpdate, now time.Time) {
	p.loading = false
	if result.err != nil {
		p.problem = safeDiagnostic(result.err.Error())
		p.refreshAt = now.Add(30 * time.Second)
	}
	if result.report != nil {
		p.problem = ""
		p.refreshAt = now.Add(30 * time.Second)
		p.report = result.report
		for _, w := range p.report.SubscriptionUsage {
			if w.ResetsAt.After(now) && w.ResetsAt.Before(p.refreshAt) {
				p.refreshAt = w.ResetsAt
			}
		}
	}
	if result.target.view != "" {
		p.rendered, p.rows = result.target, result.rows
	} else if result.report != nil {
		p.rendered, p.rows = analysisTarget{}, nil
	}
}

func renderAnalysisScreen(u UIState, l layout, view ViewMode) Frame {
	f := Frame{Width: l.width, Height: l.height, View: view}
	p := u.analysis
	if l.height == 0 {
		return f
	}
	var rows []line
	labels := analysisPainter{theme: u.Theme, ja: p.ja}
	arrows := u.Theme.symbol("↑↓", "Up/Down")
	footer := ""
	if p.picker != nil {
		l.text = max(1, l.width-2)
		picker := *p.picker
		footer = arrows + labels.tr(" select   Enter open   Esc chat", " 選択   Enter 開く   Esc チャット")
		if picker.Kind == "analyze" && p.ja {
			picker.Title = "分析"
			picker.Options = make([]string, len(p.picker.Options))
			for i, v := range p.picker.Options {
				picker.Options[i] = analysisViewName(v, true)
			}
		}
		if picker.Kind == "export" {
			picker.Title = analysisQuestion("Export", p.ja)
			picker.Hint = labels.tr("metadata/statistics only", "メタデータと統計")
			footer = labels.tr("Enter export   Esc views", "Enter 書き出す   Esc 一覧")
		}
		rows, f.PickerOffset = pickerRows(&picker, u, l, max(1, l.height-1))
	} else {
		rows = append(rows, textLine(analysisQuestion(p.view, p.ja), strong))
		title := labels.tr("Analysis", "分析") + u.Theme.symbol(" · ", " / ") + analysisViewName(p.view, p.ja)
		footer = title + "   " + labels.tr("Esc/Enter views", "Esc/Enter 一覧")
		if p.view == "Time" {
			if p.raw {
				footer += labels.tr("   E time", "   E 時間")
			} else {
				footer += labels.tr("   E events", "   E イベント")
			}
		}
		footer += "   " + arrows + "/PgUp/PgDn"
		if p.rendered.view == p.view && p.rendered.width == l.width && p.rendered.raw == p.raw {
			f.SheetHeight = max(0, l.height-2)
			f.SheetOffset = min(p.offset, max(0, len(p.rows)-max(1, f.SheetHeight)))
			rows = append(rows, p.rows[f.SheetOffset:min(len(p.rows), f.SheetOffset+f.SheetHeight)]...)
		}
	}
	if p.problem != "" && len(rows) < l.height-1 {
		rows = append(rows, textLine(p.problem, warningStyle))
	} else if (p.report == nil || p.picker == nil && len(rows) <= 1) && len(rows) < l.height-1 {
		rows = append(rows, textLine(labels.tr("Reading analysis…", "分析を読み込み中…"), meta))
	}
	for _, row := range rows[:min(len(rows), l.height)] {
		f.Lines = append(f.Lines, row.clip(l.width).paint(u.Theme))
	}
	if l.height > 1 {
		for len(f.Lines) < l.height-1 {
			f.Lines = append(f.Lines, "")
		}
		f.Lines = append(f.Lines, textLine(footer, meta).clip(l.width).paint(u.Theme))
	}
	f.CursorY = len(f.Lines) - 1
	return f
}

func (p *analysisScreen) key(key Key, frame Frame) (export string, close bool) {
	if key.Paste {
		return "", false
	}
	if p.picker != nil {
		switch key.Name {
		case "escape":
			if p.picker.Kind != "export" {
				return "", true
			}
			p.exportFormat = ""
			p.picker = analyzePicker("Export")
		case "up":
			p.picker.move(-1)
		case "down", "tab":
			p.picker.move(1)
		case "pageup":
			p.picker.move(-max(1, frame.Height-3))
		case "pagedown":
			p.picker.move(max(1, frame.Height-3))
		case "enter":
			option := p.picker.Options[p.picker.Selection]
			if p.picker.Kind == "export" {
				return option, false
			}
			if option == "Export" {
				p.picker = &Picker{Kind: "export", Title: "Export · metadata/statistics only", Options: []string{"JSON", "Markdown"}}
			} else {
				p.view, p.picker, p.offset, p.raw = option, nil, 0, false
			}
		}
		return "", false
	}
	if key.Name == "escape" || key.Name == "enter" {
		p.picker, p.view = analyzePicker(p.view), ""
		return "", false
	}
	if p.view == "Time" && (key.Text == "e" || key.Text == "E") {
		p.raw, p.offset = !p.raw, 0
		return "", false
	}
	step := max(1, frame.SheetHeight)
	delta := map[string]int{"up": -1, "down": 1, "pageup": -step, "pagedown": step, "wheelup": -3, "wheeldown": 3}[key.Name]
	p.offset = max(0, p.offset+delta)
	return "", false
}
