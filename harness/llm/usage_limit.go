package llm

import "time"

// UsageLimitFailure carries only the reset time, never a provider response body.
type UsageLimitFailure interface {
	UsageLimitReset() (time.Time, bool)
}

func UsageLimitWaiting(until time.Time) string {
	return "usage limit — waiting until " + until.UTC().Format(time.RFC3339)
}
