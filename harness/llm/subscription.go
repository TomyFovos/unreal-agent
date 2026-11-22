package llm

import (
	"slices"
	"time"
)

// SubscriptionObservation pairs a provider's allowance window with the time
// its activity was recorded in canonical history.
type SubscriptionObservation struct {
	Provider   string
	ObservedAt time.Time `json:",omitzero"`
	SubscriptionUsage
}

func (o SubscriptionObservation) ResetPassed(now time.Time) bool {
	return !o.ResetsAt.IsZero() && !now.Before(o.ResetsAt)
}

// SubscriptionObservations retains one observation per provider and window.
// A producer can report a subset of its windows in each activity.
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

func (latest *SubscriptionObservations) Merge(observations SubscriptionObservations) {
	for _, o := range observations {
		latest.add(o)
	}
}

func (latest *SubscriptionObservations) add(o SubscriptionObservation) {
	i := slices.IndexFunc(*latest, func(prior SubscriptionObservation) bool {
		return prior.Provider == o.Provider && prior.Window == o.Window
	})
	if i < 0 {
		*latest = append(*latest, o)
	} else if !o.ObservedAt.Before((*latest)[i].ObservedAt) {
		(*latest)[i] = o
	}
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
