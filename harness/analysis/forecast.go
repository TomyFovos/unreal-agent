package analysis

import (
	"math"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const (
	EMAAlpha           = 0.35
	TokenForecastHours = 3
	BurnLookback       = 3 * time.Hour
)

type TokenTrend struct {
	Hourly, Trend, Projection []Sample
}

// HourlyTokens uses clock-hour buckets. The current, incomplete bucket is
// plotted but excluded from the EMA. Gaps break the trend.
func HourlyTokens(samples []UsageSample, start, now time.Time, complete bool, model *ModelKey) (weighted TokenTrend, cached []Sample) {
	const hours = 12
	current := now.Truncate(time.Hour)
	first := current.Add(-(hours - 1) * time.Hour)
	var unknownTime bool
	var buckets [hours]TokenMix
	for _, s := range samples {
		if model != nil && (s.Provider != model.Provider || s.Model != model.Model) {
			continue
		}
		if s.At.IsZero() {
			unknownTime = true
			continue
		}
		if s.At.Before(first) || s.At.After(now) {
			continue
		}
		buckets[int(s.At.Sub(first)/time.Hour)].Add(s.Usage)
	}
	var ema float64
	var consecutive int
	for i, b := range buckets {
		at := first.Add(time.Duration(i) * time.Hour)
		// An empty hour in a complete, dated prefix is a measured zero.
		zero := complete && !unknownTime && !start.IsZero() && !start.After(at)
		value := func(m Metric) Sample {
			known := m.Known && m.Unknown == 0 || !m.Known && m.Unknown == 0 && zero
			return Sample{At: at, Value: m.Value, Known: known}
		}
		w := value(b.Weighted)
		weighted.Hourly = append(weighted.Hourly, w)
		cached = append(cached, value(b.Read))
		if i == hours-1 {
			break
		}
		if !w.Known {
			consecutive = 0
		} else if consecutive == 0 {
			ema, consecutive = w.Value, 1
		} else {
			ema = EMAAlpha*w.Value + (1-EMAAlpha)*ema
			consecutive++
		}
		weighted.Trend = append(weighted.Trend, Sample{At: at, Value: ema, Known: consecutive > 0})
	}
	if consecutive >= 2 && complete && !unknownTime {
		weighted.Projection = append(weighted.Projection, Sample{At: current.Add(-time.Hour), Value: ema, Known: true})
		for h := 0; h <= TokenForecastHours; h++ {
			weighted.Projection = append(weighted.Projection, Sample{At: current.Add(time.Duration(h) * time.Hour), Value: ema, Known: true})
		}
	}
	return weighted, cached
}

type AllowanceForecast struct {
	History, Projection []Sample
	Rate                Metric // percentage points per hour
	ExhaustedAt         time.Time
}

func (r Report) ForecastAllowance(w SubscriptionWindow) AllowanceForecast {
	var f AllowanceForecast
	at := w.PercentTime()
	if w.UsedPercent == nil || !validPercent(*w.UsedPercent) || at.IsZero() || !w.HasWindow() || w.ResetPassed(at) {
		return f
	}
	measurements := contextMeasurements(r)
	if measurements != nil {
		cutoff := at.Add(-BurnLookback)
		if p := w.PeriodMinutes(); p > 0 && !w.ResetsAt.IsZero() {
			if start := w.ResetsAt.Add(-time.Duration(p) * time.Minute); start.After(cutoff) {
				cutoff = start
			}
		}
		for _, o := range measurements.Totals.Subscription {
			if o.At.Before(cutoff) || !o.At.Before(at) || o.UsedPercent.Value == nil || !validPercent(*o.UsedPercent.Value) {
				continue
			}
			prior := llm.SubscriptionObservation{Provider: o.Provider, SubscriptionUsage: llm.SubscriptionUsage{Window: o.Window}}
			if o.WindowMinutes.Value != nil {
				prior.WindowMinutes = new(int64(*o.WindowMinutes.Value))
			}
			if o.ResetsAt != nil {
				prior.ResetsAt = *o.ResetsAt
			}
			if !w.SameCycle(prior) || prior.ResetPassed(o.At) {
				continue
			}
			f.History = append(f.History, Sample{At: o.At, Value: *o.UsedPercent.Value, Known: true})
		}
	}
	f.History = append(f.History, Sample{At: at, Value: *w.UsedPercent, Known: true})
	slices.SortStableFunc(f.History, func(a, b Sample) int { return a.At.Compare(b.At) })
	// Preserve just the current monotone run and use one sample at each time.
	dst := f.History[:0]
	for _, sample := range f.History {
		if len(dst) > 0 && sample.At.Equal(dst[len(dst)-1].At) {
			dst[len(dst)-1] = sample
			continue
		}
		dst = append(dst, sample)
	}
	start := 0
	for i := 1; i < len(dst); i++ {
		if dst[i].Value < dst[i-1].Value {
			start = i
		}
	}
	f.History = dst[start:]
	// EMA of each interval's rate in percentage points per hour. Weight
	// by elapsed wall time and normalize over the observed intervals so a
	// short first interval doesn't initialize the trend as an hour of data.
	var weightedRate, mass float64
	for i := 1; i < len(f.History); i++ {
		a, b := f.History[i-1], f.History[i]
		hours := b.At.Sub(a.At).Hours()
		rate := (b.Value - a.Value) / hours
		alpha := -math.Expm1(hours * math.Log1p(-EMAAlpha))
		weightedRate = (1-alpha)*weightedRate + alpha*rate
		mass = (1-alpha)*mass + alpha
	}
	if mass > 0 {
		f.Rate.add(weightedRate/mass, "derived")
	}
	last := f.History[len(f.History)-1]
	if !f.Rate.Known {
		return f
	}
	if f.Rate.Value > 0 {
		hours := (100 - last.Value) / f.Rate.Value
		if nanos := hours * float64(time.Hour); nanos < math.MaxInt64 {
			f.ExhaustedAt = last.At.Add(time.Duration(nanos))
		}
	}
	through := w.ResetsAt
	if !f.ExhaustedAt.IsZero() && (through.IsZero() || f.ExhaustedAt.Before(through)) {
		through = f.ExhaustedAt
	}
	if through.After(last.At) {
		used := min(100, last.Value+f.Rate.Value*through.Sub(last.At).Hours())
		f.Projection = []Sample{last, {At: through, Value: used, Known: true}}
	}
	return f
}

func validPercent(v float64) bool { return v >= 0 && v <= 100 }
