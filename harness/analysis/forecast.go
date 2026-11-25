package analysis

import (
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/usage"
)

const (
	EMAAlpha           = usage.EMAAlpha
	TokenForecastHours = 3
	BurnLookback       = usage.BurnLookback
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
	var observations []llm.SubscriptionObservation
	if measurements := contextMeasurements(r); measurements != nil {
		for _, o := range measurements.Totals.Subscription {
			prior := llm.SubscriptionObservation{Provider: o.Provider, ObservedAt: o.At,
				SubscriptionUsage: llm.SubscriptionUsage{Window: o.Window, UsedPercent: o.UsedPercent.Value}}
			if o.WindowMinutes.Value != nil {
				prior.WindowMinutes = new(int64(*o.WindowMinutes.Value))
			}
			if o.ResetsAt != nil {
				prior.ResetsAt = *o.ResetsAt
			}
			observations = append(observations, prior)
		}
	}
	forecast := usage.ForecastAllowance(w.SubscriptionObservation, observations)
	f := AllowanceForecast{ExhaustedAt: forecast.ExhaustedAt}
	if forecast.Rate != nil {
		f.Rate.add(*forecast.Rate, "derived")
	}
	for _, sample := range forecast.History {
		f.History = append(f.History, Sample{At: sample.At, Value: sample.Value, Known: true})
	}
	for _, sample := range forecast.Projection {
		f.Projection = append(f.Projection, Sample{At: sample.At, Value: sample.Value, Known: true})
	}
	return f
}

func validPercent(v float64) bool { return v >= 0 && v <= 100 }
