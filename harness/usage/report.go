package usage

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/displaytext"
)

type Allowance struct {
	llm.SubscriptionObservation
	Forecast AllowanceForecast
}

func Allowances(now time.Time, latest llm.SubscriptionObservations, history []llm.SubscriptionObservation) []Allowance {
	var out []Allowance
	for _, o := range latest {
		if !validPercent(o.UsedPercent) || !o.HasWindow() || o.ResetPassed(now) {
			continue
		}
		f := ForecastAllowance(o, history)
		// Samples support calculation and graphs. The ledger needs the rate
		// and exhaustion time and carries only those forecast results.
		f.History, f.Projection = nil, nil
		out = append(out, Allowance{SubscriptionObservation: o, Forecast: f})
	}
	slices.SortFunc(out, func(a, b Allowance) int {
		return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Window, b.Window))
	})
	return out
}

func ProviderLines(providers []Group, allowances []Allowance) []string {
	var out []string
	for _, row := range providers {
		line := fmt.Sprintf("%s · %d calls · charge %s", Label(row.Provider), row.Calls, row.DollarText())
		var windows []string
		for _, a := range allowances {
			if row.Provider != a.Provider {
				continue
			}
			window := fmt.Sprintf("%s %.1f%% used", Label(a.Window), *a.UsedPercent)
			if a.Forecast.Rate != nil {
				window += fmt.Sprintf(" (%.2f pp/h", *a.Forecast.Rate)
				if !a.Forecast.ExhaustedAt.IsZero() {
					window += ", exhaustion " + a.Forecast.ExhaustedAt.Format(time.RFC3339)
				}
				window += ")"
			}
			windows = append(windows, window)
		}
		if len(windows) > 0 {
			line += " · " + strings.Join(windows, ", ")
		}
		out = append(out, line)
	}
	return out
}

func Label(text string) string {
	if text = strings.Join(strings.Fields(displaytext.Clean(text)), " "); text != "" {
		return text
	}
	return "unknown"
}

type Spend struct {
	Provider, Currency        string
	Today, Hourly             *float64 `json:",omitempty"`
	TodayPartial, RatePartial bool     `json:",omitempty"`
}

type spendKey struct{ provider, currency string }

const rateBucket = 10 * time.Minute
const rateBuckets = int(BurnLookback / rateBucket)

type SpendBuilder struct {
	now, today   time.Time
	start        time.Time
	known        map[spendKey]*spendBuckets
	unknownToday map[string]int
	unknownRate  map[string]int
}

type spendBuckets struct {
	today    Counter[float64]
	usage    [rateBuckets]float64
	observed bool
}

func NewSpendBuilder(now time.Time) *SpendBuilder {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return &SpendBuilder{now: now, today: day, known: make(map[spendKey]*spendBuckets),
		unknownToday: make(map[string]int), unknownRate: make(map[string]int)}
}

func (b *SpendBuilder) Add(s Sample) {
	if s.At.IsZero() || s.At.After(b.now) {
		return
	}
	if b.start.IsZero() || s.At.Before(b.start) {
		b.start = s.At
	}
	today := !s.At.Before(b.today)
	age := b.now.Sub(s.At)
	recent := age < BurnLookback
	if s.Charge == nil || s.Charge.Charged == nil || s.Charge.Currency == "" {
		if today {
			b.unknownToday[s.Provider] += s.Calls
		}
		if recent {
			b.unknownRate[s.Provider] += s.Calls
		}
		return
	}
	key := spendKey{s.Provider, s.Charge.Currency}
	v := b.known[key]
	if v == nil {
		v = &spendBuckets{}
		b.known[key] = v
	}
	if today {
		v.today.add(s.Charge.Charged, s.Calls)
	}
	if recent {
		v.usage[rateBuckets-1-int(age/rateBucket)] += *s.Charge.Charged
		v.observed = true
	}
}

func (b *SpendBuilder) Spend(partial bool) []Spend {
	var out []Spend
	for _, key := range slices.SortedFunc(maps.Keys(b.known), func(a, b spendKey) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.currency, b.currency))
	}) {
		v := b.known[key]
		row := Spend{Provider: key.provider, Currency: key.currency, Today: v.today.Value,
			TodayPartial: partial || b.unknownToday[key.provider] > 0,
			RatePartial:  partial || b.unknownRate[key.provider] > 0}
		if row.Today == nil && !row.TodayPartial {
			row.Today = new(float64(0))
		}
		if v.observed || !row.RatePartial {
			var weighted, mass float64
			earliest := b.now.Add(-BurnLookback)
			for i, amount := range v.usage {
				from := earliest.Add(time.Duration(i) * rateBucket)
				to := from.Add(rateBucket)
				if from.Before(b.start) {
					from = b.start
				}
				hours := to.Sub(from).Hours()
				if hours <= 0 {
					continue
				}
				alpha := -math.Expm1(hours * math.Log1p(-EMAAlpha))
				weighted = (1-alpha)*weighted + alpha*amount/hours
				mass = (1-alpha)*mass + alpha
			}
			if mass > 0 {
				row.Hourly = new(weighted / mass)
			}
		}
		out = append(out, row)
	}
	return out
}
