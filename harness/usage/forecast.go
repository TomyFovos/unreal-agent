package usage

import (
	"math"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type PercentSample struct {
	At    time.Time
	Value float64
}

type AllowanceForecast struct {
	History, Projection []PercentSample `json:",omitempty"`
	Rate                *float64        `json:",omitempty"` // percentage points per hour
	ExhaustedAt         time.Time       `json:",omitzero"`
}

// ForecastAllowance shares the same elapsed-time EMA between /analyze and the ledger.
func ForecastAllowance(w llm.SubscriptionObservation, observations []llm.SubscriptionObservation) AllowanceForecast {
	var f AllowanceForecast
	at := w.PercentTime()
	if !validPercent(w.UsedPercent) || at.IsZero() || !w.HasWindow() || w.ResetPassed(at) {
		return f
	}
	cutoff := at.Add(-BurnLookback)
	if p := w.PeriodMinutes(); p > 0 && !w.ResetsAt.IsZero() {
		if start := w.ResetsAt.Add(-time.Duration(p) * time.Minute); start.After(cutoff) {
			cutoff = start
		}
	}
	for _, o := range observations {
		if o.ObservedAt.Before(cutoff) || !o.ObservedAt.Before(at) || !validPercent(o.UsedPercent) ||
			!w.SameCycle(o) || o.ResetPassed(o.ObservedAt) {
			continue
		}
		f.History = append(f.History, PercentSample{At: o.ObservedAt, Value: *o.UsedPercent})
	}
	f.History = append(f.History, PercentSample{At: at, Value: *w.UsedPercent})
	slices.SortStableFunc(f.History, func(a, b PercentSample) int { return a.At.Compare(b.At) })
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
		f.Rate = new(weightedRate / mass)
	}
	last := f.History[len(f.History)-1]
	if f.Rate == nil {
		return f
	}
	if (*f.Rate) > 0 {
		hours := (100 - last.Value) / (*f.Rate)
		if nanos := hours * float64(time.Hour); nanos < math.MaxInt64 {
			f.ExhaustedAt = last.At.Add(time.Duration(nanos))
		}
	}
	through := w.ResetsAt
	if !f.ExhaustedAt.IsZero() && (through.IsZero() || f.ExhaustedAt.Before(through)) {
		through = f.ExhaustedAt
	}
	if through.After(last.At) {
		used := min(100, last.Value+(*f.Rate)*through.Sub(last.At).Hours())
		f.Projection = []PercentSample{last, {At: through, Value: used}}
	}
	return f
}

func validPercent(v *float64) bool { return v != nil && *v >= 0 && *v <= 100 }
