package tui

import (
	"time"

	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// nextRender schedules the next visible clock change. Input, resize and
// canonical updates can always wake the loop independently of this deadline.
func nextRender(s Snapshot, u UIState, view ViewMode) time.Time {
	deadline := u.Notification.Until
	consider := func(next time.Time) {
		if next.After(u.Now) && (deadline.IsZero() || next.Before(deadline)) {
			deadline = next
		}
	}
	if p := u.analysis; p != nil {
		if !p.loading {
			if p.report == nil || p.version != s.Version || p.viewerVersion != u.Viewer.Version || !p.refreshAt.IsZero() && !u.Now.Before(p.refreshAt) {
				consider(p.nextRead)
			}
			consider(p.refreshAt)
		}
		return deadline
	}
	consider(nextSubscriptionReset(s, u))
	if u.cache != nil {
		u.index = u.cache.index
	}
	working := s.Connected && s.Running && s.Failure == "" && (mainBusy(s) || workingChildren(s, u) != 0)
	usageWait := s.Progress != nil && s.Progress.Mode == "usage_limit_wait"
	if u.Busy || !s.Connected || working && !usageWait {
		interval := time.Second
		if !u.Theme.Still {
			interval = 120 * time.Millisecond
		}
		consider(u.Now.Truncate(interval).Add(interval))
		return deadline
	}

	for _, row := range u.Viewer.Rows {
		if row.Elapsed.Known && !row.CreatedAt.IsZero() && row.Runtime == viewer.RuntimeRunning && row.Finish == nil && row.End == nil {
			age := u.Now.Sub(row.CreatedAt)
			step := time.Minute
			if age < time.Minute {
				step = time.Second
			}
			boundary := row.CreatedAt.Add(age.Truncate(step) + step)
			if boundary.Nanosecond() != 0 {
				boundary = boundary.Truncate(time.Second).Add(time.Second)
			}
			consider(boundary)
		}
		if view == ViewChat {
			continue
		}
		if !u.Theme.Still && row.ModelPhase != "" && childVisualState(row, s) == stateWorking {
			consider(u.Now.Truncate(120 * time.Millisecond).Add(120 * time.Millisecond))
		}
		// Activity moves across the same rolling minute buckets as the cards.
		// Coalesce nearby boundaries at the display's one-second resolution.
		bucket := func(at time.Time) {
			age := u.Now.Sub(at)
			if !at.IsZero() && age >= 0 && age <= 10*time.Minute {
				boundary := at.Add(age.Truncate(time.Minute) + time.Minute)
				consider(boundary.Truncate(time.Second).Add(time.Second))
			}
		}
		bucket(row.CreatedAt)
		for _, at := range row.RecentActivity {
			bucket(at)
		}
	}
	return deadline
}
