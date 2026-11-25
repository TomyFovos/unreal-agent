package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	ledgerusage "github.com/unreallabsai/unreal-agent/harness/usage"
)

func watchSpend(ctx context.Context, load func(context.Context, time.Duration) (subagent.UsageReport, error), updates chan<- []ledgerusage.Spend) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		if report, err := load(ctx, time.Hour); err == nil {
			select {
			case updates <- report.Spend:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func spendBar(rows []ledgerusage.Spend, theme Theme, room int) line {
	var out line
	for _, row := range rows {
		if row.Today == nil {
			continue
		}
		rate := theme.symbol("—", "-")
		if row.Hourly != nil {
			rate = ledgerusage.Money(row.Currency, *row.Hourly, 1)
			if row.RatePartial {
				rate = "~" + rate
			}
		}
		today := ledgerusage.Money(row.Currency, *row.Today, 1)
		if row.TodayPartial {
			today += "+"
		}
		separator := theme.symbol(" · ", " / ")
		part := textLine(separator+analysis.SubscriptionProviderName(ledgerusage.Label(row.Provider))+" 今日 "+today+separator+rate+"/h", meta)
		if part.width() <= room {
			out = append(out, part...)
			room -= part.width()
		}
	}
	return out
}

func (p *analysisPainter) charges(r *subagent.UsageReport) {
	p.section("Reported charges", "報告された課金")
	for _, summary := range r.All.ByProvider {
		p.add(analysis.SubscriptionProviderName(ledgerusage.Label(summary.Provider))+" · "+summary.DollarText(), strong)
		for _, group := range r.All.ByModelRole {
			if group.Provider != summary.Provider {
				continue
			}
			label := strings.Join([]string{ledgerusage.Label(group.Model), ledgerusage.Label(group.Role)}, " / ")
			if len(group.Charges) == 0 {
				p.add("  "+label+" · "+p.tr("charge unknown", "課金不明"), meta)
				continue
			}
			for _, amount := range group.Charges {
				source := p.tr("all sources", "全課金区分")
				if amount.BillingSource != "" {
					source = ledgerusage.Label(amount.BillingSource)
				}
				charged, perCall := p.tr("unknown", "不明"), p.tr("unknown", "不明")
				if amount.Charged.Value != nil {
					charged = ledgerusage.Money(amount.Currency, *amount.Charged.Value, 3)
					if group.ChargeUnknown > 0 {
						charged += "+"
					}
				}
				if rate := amount.PerCall(); rate != nil {
					perCall = ledgerusage.Money(amount.Currency, *rate, 6)
				}
				p.add(fmt.Sprintf("  %s · %s · %s · %s%s", label, source, charged, perCall, p.tr("/call", "/回")), normal)
			}
		}
		if slices.ContainsFunc(r.Allowances, func(a ledgerusage.Allowance) bool { return a.Provider == summary.Provider }) {
			continue
		}
		for _, spend := range r.Spend {
			if spend.Provider == summary.Provider && spend.Hourly != nil {
				text := ledgerusage.Money(spend.Currency, *spend.Hourly, 2)
				if spend.RatePartial {
					text += "+"
				}
				p.add("  EMA "+text+"/h", meta)
			}
		}
	}
	if len(r.All.ByProvider) == 0 {
		p.add(p.tr("Charge unknown", "課金不明"), meta)
	}
	p.blank()
}
