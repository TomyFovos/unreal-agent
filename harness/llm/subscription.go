package llm

import (
	"slices"
	"time"
)

// SubscriptionObservation pairs a provider's allowance window with the time
// its activity was recorded in canonical history.
type SubscriptionObservation struct {
	Provider             string
	ObservedAt           time.Time `json:",omitzero"`
	PercentObservedAt    time.Time `json:",omitzero"` // when a later status carries an older percentage
	PercentInvalidatedAt time.Time `json:",omitzero"` // percentages through this observation are stale
	SubscriptionUsage
}

func (o SubscriptionObservation) PercentTime() time.Time {
	if !o.PercentObservedAt.IsZero() {
		return o.PercentObservedAt
	}
	return o.ObservedAt
}

func (o SubscriptionObservation) PeriodMinutes() int64 {
	if o.WindowMinutes != nil && *o.WindowMinutes > 0 {
		return *o.WindowMinutes
	}
	if o.Provider == "claude" {
		switch o.Window {
		case "five_hour":
			return 5 * 60
		case "seven_day", "seven_day_opus", "seven_day_sonnet":
			return 7 * 24 * 60
		}
	}
	return 0
}

func (o SubscriptionObservation) HasWindow() bool {
	return o.PeriodMinutes() > 0 || !o.ResetsAt.IsZero()
}

// SameCycle permits the provider to round a reset's Unix timestamp by a
// second between requests. The period is part of the window's identity.
func (o SubscriptionObservation) SameCycle(other SubscriptionObservation) bool {
	const resetJitter = 2 * time.Second
	return o.Provider == other.Provider && o.Window == other.Window && o.PeriodMinutes() == other.PeriodMinutes() &&
		o.HasWindow() && (o.ResetsAt.IsZero() && other.ResetsAt.IsZero() ||
		!o.ResetsAt.IsZero() && !other.ResetsAt.IsZero() && o.ResetsAt.Sub(other.ResetsAt).Abs() <= resetJitter)
}

func (o SubscriptionObservation) ResetPassed(now time.Time) bool {
	return !o.ResetsAt.IsZero() && !now.Before(o.ResetsAt)
}

// SubscriptionObservations retains one observation per provider and window.
// Claude's latest successful response has an empty window so it can also
// supersede a rejection observed by another Session.
type SubscriptionObservations []SubscriptionObservation

func (latest *SubscriptionObservations) Observe(activity ActivityRecord, at time.Time) {
	if activity.Kind != ActivitySubscriptionUsage || activity.Provider == "" {
		return
	}
	for _, usage := range activity.Subscription {
		if usage.Window != "" {
			latest.add(SubscriptionObservation{Provider: activity.Provider, ObservedAt: at, SubscriptionUsage: usage})
		}
	}
}

func (latest *SubscriptionObservations) ObserveClaudeSuccess(at time.Time) {
	if !at.IsZero() {
		latest.add(SubscriptionObservation{Provider: "claude", ObservedAt: at})
	}
}

func (latest *SubscriptionObservations) Merge(observations SubscriptionObservations) {
	for _, o := range observations {
		latest.add(o)
	}
}

func (o *SubscriptionObservation) invalidatePercent(through time.Time) {
	if through.After(o.PercentInvalidatedAt) {
		o.PercentInvalidatedAt = through
	}
	if o.UsedPercent != nil && !o.PercentTime().After(o.PercentInvalidatedAt) {
		o.UsedPercent, o.PercentObservedAt = nil, time.Time{}
	}
}

func (o *SubscriptionObservation) recoverAfterSuccess(at time.Time) {
	if o.Provider == "claude" && o.Status == "rejected" && at.After(o.ObservedAt) {
		o.invalidatePercent(o.ObservedAt)
	}
}

func (latest *SubscriptionObservations) add(o SubscriptionObservation) {
	if o.Provider == "claude" {
		if o.Window == "" {
			for i := range *latest {
				(*latest)[i].recoverAfterSuccess(o.ObservedAt)
			}
		} else if o.Status == "rejected" {
			for _, prior := range *latest {
				if prior.Provider == "claude" && prior.Window == "" {
					o.recoverAfterSuccess(prior.ObservedAt)
					break
				}
			}
		}
	}
	if o.Status == "allowed" && o.UsedPercent == nil {
		o.invalidatePercent(o.ObservedAt)
	}
	i := slices.IndexFunc(*latest, func(prior SubscriptionObservation) bool {
		return prior.Provider == o.Provider && prior.Window == o.Window
	})
	if i < 0 {
		*latest = append(*latest, o)
		return
	}
	older, newer := (*latest)[i], o
	if newer.ObservedAt.Before(older.ObservedAt) {
		older, newer = newer, older
	}
	if older.PercentInvalidatedAt.After(newer.PercentInvalidatedAt) {
		newer.invalidatePercent(older.PercentInvalidatedAt)
	}
	if (newer.Status == "rejected" || newer.Status == "allowed_warning") && older.UsedPercent != nil &&
		(newer.PercentInvalidatedAt.IsZero() || older.PercentTime().After(newer.PercentInvalidatedAt)) &&
		(newer.UsedPercent == nil || newer.PercentTime().Before(older.PercentTime())) {
		candidate := newer
		// A status-only update can omit metadata. Borrow it while the last
		// identified cycle is still open, or when a supplied reset matches.
		if newer.UsedPercent == nil {
			if candidate.ResetsAt.IsZero() && older.ResetsAt.After(newer.ObservedAt) {
				candidate.ResetsAt = older.ResetsAt
			}
			if candidate.PeriodMinutes() == 0 && !candidate.ResetsAt.IsZero() {
				candidate.WindowMinutes = older.WindowMinutes
			}
		}
		if candidate.SameCycle(older) && !older.ResetPassed(newer.ObservedAt) &&
			(!older.ResetsAt.IsZero() || candidate.PeriodMinutes() > 0 &&
				newer.ObservedAt.Sub(older.PercentTime()).Minutes() < float64(candidate.PeriodMinutes())) {
			newer = candidate
			newer.UsedPercent, newer.PercentObservedAt = older.UsedPercent, older.PercentTime()
		}
	}
	(*latest)[i] = newer
}

func (observations SubscriptionObservations) Clone() SubscriptionObservations {
	out := slices.Clone(observations)
	for i := range out {
		if out[i].UsedPercent != nil {
			out[i].UsedPercent = new(*out[i].UsedPercent)
		}
		if out[i].WindowMinutes != nil {
			out[i].WindowMinutes = new(*out[i].WindowMinutes)
		}
	}
	return out
}
