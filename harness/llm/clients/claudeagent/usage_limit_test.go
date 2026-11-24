//go:build linux || darwin

package claudeagent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestClaudeSubscriptionResetText(t *testing.T) {
	now := time.Date(2026, time.November, 19, 20, 23, 0, 0, time.UTC)
	local := now.In(time.FixedZone("local", 9*60*60))
	for _, tc := range []struct {
		name, text, want string
	}{
		{"weekly next day", "You've hit your weekly limit · resets 3pm (UTC)", "2026-11-20T15:00:00Z"},
		{"legacy session", "You've hit your limit · resets 12am (UTC)", "2026-11-20T00:00:00Z"},
		{"session timezone", "You've hit your session limit · resets 3:50pm (America/New_York)", "2026-11-19T20:50:00Z"},
		{"local 24-hour clock", "You've hit your 5-hour limit, resets 01:40", "2026-11-20T16:40:00Z"},
		{"weekday", "You've hit your weekly limit · resets Mon 12:00am (UTC)", "2026-11-23T00:00:00Z"},
		{"calendar date comma", "You've hit your weekly limit · resets Nov 24, 2am (UTC)", "2026-11-24T02:00:00Z"},
		{"calendar date at", "You've hit your weekly limit · resets Nov 24 at 7pm", "2026-11-24T10:00:00Z"},
		{"calendar year rollover", "You've hit your weekly limit · resets Jan 2, 12pm (UTC)", "2027-01-02T12:00:00Z"},
		{"ISO offset", "You've hit your Opus limit · resets 2026-11-21T00:00:00+09:00", "2026-11-20T15:00:00Z"},
		{"ISO in a stated timezone", "You've hit your Sonnet limit · resets 2026-11-21T00:00 (Asia/Tokyo)", "2026-11-20T15:00:00Z"},
		{"curly apostrophe and mixed case", "You’ve hit your session limit — resets at 12:00 PM (UTC)\n/usage-credits", "2026-11-20T12:00:00Z"},
		{"invalid timezone", "You've hit your 5-hour limit · resets 01:40 (No/Such_Zone)", ""},
		{"invalid clock", "You've hit your session limit · resets 25:00", ""},
		{"without a time", "You've reached your Fable limit. Run /usage-credits to continue or switch models with /model.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset, reached := subscriptionLimitNotice(tc.text)
			if !reached {
				t.Fatal("missed subscription failure")
			}
			at := parseSubscriptionReset(reset, local)
			if tc.want == "" && !at.IsZero() || tc.want != "" && at.Format(time.RFC3339) != tc.want {
				t.Fatalf("reset = %v, want %q", at, tc.want)
			}
		})
	}
}

func TestClaudeRecordsSeparateSubscriptionWindows(t *testing.T) {
	fake := testclaude.New(t)
	fake.Set(t, testclaude.Config{AgentReply: []jsontext.Value{
		jsontext.Value(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1800000000,"rateLimitType":"five_hour","utilization":0.71}}`),
		jsontext.Value(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1800600000,"rateLimitType":"seven_day","utilization":0.31}}`),
		jsontext.Value(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"seven_day_opus"}}`),
		jsontext.Value(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`),
	}})
	client, err := NewClient(Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()})
	if err != nil {
		t.Fatal(err)
	}
	var windows []llm.SubscriptionUsage
	_, err = client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("work")}}, llm.RequestOptions{
		Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil },
		RecordActivity: func(_ context.Context, a llm.ActivityRecord) error {
			if a.Kind == llm.ActivitySubscriptionUsage {
				if a.Provider != ProviderID || len(a.Subscription) != 1 {
					t.Errorf("invalid Claude subscription observation: %+v", a)
				}
				windows = append(windows, a.Subscription...)
			}
			return nil
		},
	})
	if err != nil || len(windows) != 3 {
		t.Fatal("missed an independent Claude window", err, windows)
	}
	for i, want := range []struct {
		window, status string
		used           float64
		reset          int64
	}{
		{"five_hour", "allowed", 71, 1800000000},
		{"seven_day", "allowed_warning", 31, 1800600000},
	} {
		w := windows[i]
		if w.Window != want.window || w.Status != want.status || w.UsedPercent == nil || *w.UsedPercent != want.used || w.ResetsAt.Unix() != want.reset || w.ResetsAt.Location() != time.UTC {
			t.Fatalf("wrong Claude window %d: %+v", i, w)
		}
	}
	if w := windows[2]; w.Window != "seven_day_opus" || w.Status != "allowed" || w.UsedPercent != nil || !w.ResetsAt.IsZero() {
		t.Fatalf("unreported fields gained a value: %+v", w)
	}
}

func TestClaudeSubscriptionLimitStream(t *testing.T) {
	fake := testclaude.New(t)
	// The rejection and synthetic-assistant shapes come from the 2.1.x
	// print-mode reproductions in anthropics/claude-code#57096 and #103820.
	rejected := jsontext.Value(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792494000,"rateLimitType":"seven_day"}}`)
	opus := jsontext.Value(`{"type":"assistant","error":"rate_limit","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your Opus limit · resets 2026-11-20T15:00:00Z"}]}}`)
	opusResult := jsontext.Value(`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your Opus limit · resets 2026-11-20T15:00:00Z"}`)
	otherFailure := jsontext.Value(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["connection closed"]}`)
	opusReset := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		frames  []jsontext.Value
		reached bool
		until   time.Time
		message string
		api     bool
		success bool
	}{
		{name: "epoch wins over text", frames: []jsontext.Value{rejected, opusResult}, reached: true, until: time.Unix(1792494000, 0)},
		{name: "epoch without result", frames: []jsontext.Value{rejected}, reached: true, until: time.Unix(1792494000, 0)},
		{name: "assistant error text", frames: []jsontext.Value{opus, otherFailure}, reached: true, until: opusReset},
		{
			name: "result error list",
			frames: []jsontext.Value{
				jsontext.Value(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["internal error","You've hit your Opus limit · resets 2026-11-20T15:00:00Z"]}`),
			},
			reached: true, until: opusReset,
		},
		{
			name: "result usage code alone",
			frames: []jsontext.Value{
				jsontext.Value(`{"type":"result","subtype":"success","is_error":true,"api_error":"usage_limit_reached"}`),
			},
			reached: true,
		},
		{
			name: "recovered quota and later failure",
			frames: []jsontext.Value{
				rejected, jsontext.Value(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1792494000}}`), otherFailure,
			},
			message: "connection closed",
		},
		{
			name: "successful result after quota event",
			frames: []jsontext.Value{
				rejected, jsontext.Value(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`),
			},
			success: true,
		},
		{
			name: "ordinary assistant text and later failure",
			frames: []jsontext.Value{
				jsontext.Value(`{"type":"assistant","message":{"model":"claude-opus","content":[{"type":"text","text":"You've hit your Opus limit"}]}}`), otherFailure,
			},
			message: "connection closed",
		},
		{
			name: "native child error",
			frames: []jsontext.Value{
				jsontext.Value(`{"type":"assistant","parent_tool_use_id":"child","error":"rate_limit","api_error":"usage_limit_reached","message":{"content":[{"type":"text","text":"You've hit your Opus limit"}]}}`), otherFailure,
			},
			message: "connection closed",
		},
		{
			name: "generic 429",
			frames: []jsontext.Value{
				jsontext.Value(`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"Server is temporarily limiting requests"}`),
			},
			message: "Server is temporarily limiting requests",
		},
		{name: "explicit API route", frames: []jsontext.Value{rejected, opusResult}, api: true, message: "You've hit your Opus limit · resets 2026-11-20T15:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit := 1
			if tc.success {
				exit = 0
			}
			fake.Set(t, testclaude.Config{AgentReply: tc.frames, Exit: exit})
			config := Config{Binary: fake.Binary, Directory: t.TempDir(), Results: observed.NewResults()}
			client, err := NewClient(config)
			if tc.api {
				client, err = NewAPIClient(config, credential.Material{Token: credential.NewSecret("fixture-api-key")})
			}
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("work")}}, llm.RequestOptions{
				Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) { return nil, nil },
			})
			if tc.success {
				if err != nil || len(response.Output) != 1 {
					t.Fatal("successful turn did not complete", response, err)
				}
				return
			}
			var limit llm.UsageLimitFailure
			if !errors.As(err, &limit) {
				t.Fatalf("expected Claude Code error, got %v", err)
			}
			at, reached := limit.UsageLimitReset()
			if reached != tc.reached || !at.Equal(tc.until) {
				t.Fatalf("usage limit = %t at %v, want %t at %v (%v)", reached, at, tc.reached, tc.until, err)
			}
			if !tc.reached && err.Error() != "Claude Code: turn_failed: "+tc.message {
				t.Fatalf("ordinary turn failure changed: %v", err)
			}
		})
	}
}
