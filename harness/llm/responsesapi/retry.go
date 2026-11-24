package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

var retryAfterMessagePattern = regexp.MustCompile(`(?i)\btry again in\s*(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|seconds?)\b`)

var usageRetryMessagePattern = regexp.MustCompile(`(?i)\btry again in\s*(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|seconds?|m|minutes?|h|hours?|d|days?)\b`)

func usageResetBody(body []byte, now time.Time) time.Time {
	var fields struct {
		Error           jsontext.Value `json:"error"`
		ResetsAt        jsontext.Value `json:"resets_at"`
		ResetAt         jsontext.Value `json:"reset_at"`
		ResetsInSeconds float64        `json:"resets_in_seconds"`
	}
	if json.Unmarshal(body, &fields) != nil {
		return time.Time{}
	}
	if len(fields.Error) > 0 && string(fields.Error) != "null" {
		return usageResetBody(fields.Error, now)
	}
	var reset time.Time
	for _, raw := range []jsontext.Value{fields.ResetsAt, fields.ResetAt} {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			value = string(raw)
		}
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				continue
			}
			at = time.Unix(seconds, 0)
		}
		if at.After(reset) {
			reset = at
		}
	}
	if reset.IsZero() && fields.ResetsInSeconds > 0 && fields.ResetsInSeconds < float64((1<<63-1)/time.Second) {
		reset = now.Add(time.Duration(fields.ResetsInSeconds * float64(time.Second)))
	}
	return reset.UTC()
}

func usageResetHint(err *APIError, headers http.Header, now time.Time) {
	if _, limited := err.UsageLimitReset(); !limited {
		return
	}
	delay := retryAfterHeader(headers.Get("Retry-After"), now)
	if match := usageRetryMessagePattern.FindStringSubmatch(err.Message); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		unit := time.Second
		switch strings.ToLower(match[2]) {
		case "ms", "millisecond", "milliseconds":
			unit = time.Millisecond
		case "m", "minute", "minutes":
			unit = time.Minute
		case "h", "hour", "hours":
			unit = time.Hour
		case "d", "day", "days":
			unit = 24 * time.Hour
		}
		if value > 0 && value < float64(1<<63-1)/float64(unit) {
			delay = max(delay, time.Duration(value*float64(unit)))
		}
	}
	if delay > 0 && now.Add(delay).After(err.ResetAt) {
		err.ResetAt = now.Add(delay).UTC()
	}
}

// Providers report failures inconsistently, so this classifier intentionally
// fails open, favoring retries.
func retryableResponseError(err *APIError, retryableStatuses []int) bool {
	if err == nil {
		return false
	}
	if _, limited := err.UsageLimitReset(); limited {
		return false
	}
	switch err.Code {
	case "context_length_exceeded", "insufficient_quota", "usage_not_included", "usage_limit_reached",
		"credit_balance_exhausted", "billing_hard_limit_reached",
		"cyber_policy", "misalignment_policy_violation", "invalid_prompt", "bio_policy",
		"invalid_api_key", "invalid_token":
		return false
	}
	switch err.Type {
	case "authentication_error", "permission_error", "insufficient_quota":
		return false
	}
	if err.StatusCode < http.StatusOK || err.StatusCode >= http.StatusMultipleChoices {
		return slices.Contains(retryableStatuses, err.StatusCode)
	}
	return true
}

func responseRetryDelay(policy primitives.RemoteRetryPolicy, attempt int, err *APIError, headers http.Header, now time.Time, jitter float64) time.Duration {
	hint := retryAfterHeader(headers.Get("Retry-After"), now)
	if err != nil && err.Code == "rate_limit_exceeded" {
		hint = max(hint, retryAfterMessage(err.Message))
	}
	if hint > 0 {
		return min(hint, policy.MaxBackoff)
	}
	if err != nil && (err.Code == "server_is_overloaded" || err.Code == "slow_down") {
		// Unlike the Codex CLI, unattended runs retry overloads with a longer backoff.
		policy.InitialBackoff = 10 * time.Second
		policy.MaxBackoff = time.Minute
	}
	delay := policy.Backoff(attempt)
	return delay - time.Duration(float64(delay/5)*jitter)
}

func retryAfterHeader(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64((1<<63-1)/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return max(0, deadline.Sub(now))
	}
	return 0
}

func retryAfterMessage(message string) time.Duration {
	match := retryAfterMessagePattern.FindStringSubmatch(message)
	if match == nil {
		return 0
	}
	unit := "s"
	if strings.HasPrefix(strings.ToLower(match[2]), "m") {
		unit = "ms"
	}
	delay, err := time.ParseDuration(match[1] + unit)
	if err != nil {
		return 0
	}
	return delay
}
