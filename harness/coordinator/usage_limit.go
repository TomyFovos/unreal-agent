package coordinator

import (
	"context"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

const usageLimitRetryInterval = 5 * time.Minute

func (current *coordinator) scheduleUsageRetry() {
	current.modelRetry = time.After(max(0, min(usageLimitRetryInterval, time.Until(current.usageRetryAt))))
}

func (current *coordinator) waitForUsage(ctx context.Context, until time.Time) error {
	previous := current.usageRetryAt
	now := time.Now()
	if until.IsZero() {
		until = previous
	}
	if !until.After(now) {
		until = now.Add(primitives.DefaultRemoteRequest("", "", "").RetryPolicy.MaxBackoff)
	}
	current.usageRetryAt = until
	current.state.callModel = false
	current.scheduleUsageRetry()
	if previous.IsZero() {
		if publish := current.dependencies.ModelWaiting; publish != nil {
			return publish(ctx, until)
		}
	} else if update := current.dependencies.ModelWaitUpdated; update != nil && !until.Equal(previous) {
		update(until)
	}
	return nil
}

func (current *coordinator) clearUsageWait(ctx context.Context) error {
	if current.usageRetryAt.IsZero() {
		return nil
	}
	current.usageRetryAt = time.Time{}
	if publish := current.dependencies.ModelWaiting; publish != nil {
		return publish(ctx, time.Time{})
	}
	return nil
}
