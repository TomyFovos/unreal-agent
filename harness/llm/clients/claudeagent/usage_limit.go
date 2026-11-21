package claudeagent

import (
	"regexp"
	"strings"
	"time"
)

const usageLimitReached = "usage_limit_reached"

func (e *Error) UsageLimitReset() (time.Time, bool) {
	return e.resetAt, e.Code == usageLimitReached
}

type subscriptionLimit struct {
	rate             *rateLimitInfo
	assistantLimited bool
	assistantReset   string
}

// A regular assistant reply or a native subagent's failure is not evidence
// that the main turn hit the subscription limit.
func (l *subscriptionLimit) observeAssistant(f frame, m message) {
	if f.ParentToolUseID != nil {
		return
	}
	l.assistantLimited, l.assistantReset = f.usageLimitError(), ""
	if !l.assistantLimited && !f.IsAPIErrorMessage && m.Model != "<synthetic>" && string(f.Error) != `"rate_limit"` {
		return
	}
	for _, b := range m.Content {
		if b.Type == "text" {
			if reset, limited := subscriptionLimitNotice(b.Text); limited {
				l.assistantLimited, l.assistantReset = true, reset
				return
			}
		}
	}
}

func (f frame) usageLimitError() bool {
	return string(f.APIError) == `"`+usageLimitReached+`"`
}

// Consult the result only on failure. A rejected quota event supplies a more
// precise instant than the rounded time in either error message.
func (l *subscriptionLimit) failure(result *frame, now time.Time) *Error {
	rejected := l.rate != nil && l.rate.Status == "rejected"
	if rejected && l.rate.ResetsAt > 0 {
		return &Error{Code: usageLimitReached, resetAt: time.Unix(l.rate.ResetsAt, 0).UTC()}
	}
	limited := rejected || l.assistantLimited
	if result != nil {
		limited = limited || result.usageLimitError()
		for _, text := range append([]string{result.Result}, result.Errors...) {
			if reset, reached := subscriptionLimitNotice(text); reached {
				limited = true
				if at := parseSubscriptionReset(reset, now); !at.IsZero() {
					return &Error{Code: usageLimitReached, resetAt: at}
				}
			}
		}
	}
	if limited {
		return &Error{Code: usageLimitReached, resetAt: parseSubscriptionReset(l.assistantReset, now)}
	}
	return nil
}

var (
	limitNotice = regexp.MustCompile(`(?i)^(?:api error:\s*)?(?:you['’]ve (?:hit|reached) your (?:(?:session|5-hour|weekly|opus|sonnet|fable) )?limit|you['’]re out of extra usage)\b`)
	resetNotice = regexp.MustCompile(`(?i)^[\s·,;–—-]*resets(?: at)?\s+(.+)$`)
	resetDate   = regexp.MustCompile(`(?i)^(?:(Mon|Tue|Wed|Thu|Fri|Sat|Sun)|([a-z]{3} [0-9]{1,2}))(?:, | at | )`)
)

func subscriptionLimitNotice(text string) (reset string, limited bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	match := limitNotice.FindStringIndex(line)
	if match == nil {
		return "", false
	}
	if hint := resetNotice.FindStringSubmatch(strings.TrimSpace(line[match[1]:])); hint != nil {
		return strings.TrimSpace(hint[1]), true
	}
	return "", true
}

func parseSubscriptionReset(reset string, now time.Time) time.Time {
	if reset == "" {
		return time.Time{}
	}
	loc := now.Location()
	if value, zone, ok := strings.Cut(reset, " ("); ok {
		name, ok := strings.CutSuffix(zone, ")")
		if !ok {
			return time.Time{}
		}
		var err error
		loc, err = time.LoadLocation(name)
		if err != nil {
			return time.Time{}
		}
		reset = value
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if at, err := time.ParseInLocation(layout, reset, loc); err == nil {
			return at.UTC()
		}
	}
	return nextSubscriptionReset(strings.Join(strings.Fields(reset), " "), now.In(loc))
}

func nextSubscriptionReset(reset string, now time.Time) time.Time {
	// Advance the calendar date separately so a clock change does not shift
	// the stated hour when the reset falls on a later day.
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	years, days := 0, 1
	if date := resetDate.FindStringSubmatch(reset); date != nil {
		reset = reset[len(date[0]):]
		if weekday := date[1]; weekday != "" {
			for !strings.EqualFold(day.Weekday().String()[:3], weekday) {
				day = day.AddDate(0, 0, 1)
			}
			days = 7
		} else {
			monthDay, err := time.Parse("Jan 2", date[2])
			if err != nil {
				return time.Time{}
			}
			day = time.Date(day.Year(), monthDay.Month(), monthDay.Day(), 0, 0, 0, 0, time.UTC)
			if day.Day() != monthDay.Day() {
				return time.Time{}
			}
			years, days = 1, 0
		}
	}
	for _, layout := range []string{"3:04pm", "3:04 pm", "3pm", "3 pm", "15:04"} {
		if clock, err := time.Parse(layout, strings.ToLower(reset)); err == nil {
			at := func(day time.Time) time.Time {
				return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
			}
			next := at(day)
			if !next.After(now) {
				next = at(day.AddDate(years, 0, days))
			}
			return next.UTC()
		}
	}
	return time.Time{}
}
