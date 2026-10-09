//go:build linux || darwin

package claudecode

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestStructuredResultCleanupHasIndependentDeadline(t *testing.T) {
	c, f, req, opt := structuredClient(t)
	// Internal fixture injection, not a relaxation of configured timeout limits.
	c.config.ToolBridge.TimeoutMillis = 60
	f.Set(t, testclaude.Config{Subscription: "team", StructuredAfterResultMillis: 100, StructuredTrailing: "{\"type\":\"keep_alive\"}\n"})
	r, err := c.Respond(t.Context(), req, opt)
	if err != nil {
		t.Fatal("validated candidate was incorrectly charged to generation timeout", err)
	}
	if len(r.Output) != 1 {
		t.Fatal("cleanup lost validated Final")
	}
}

func TestStructuredTerminationDrainAndReap(t *testing.T) {
	for _, tc := range []struct {
		name, code, stage string
		config            testclaude.Config
	}{
		{"eof", "", "", testclaude.Config{}},
		{"allowed-metadata", "", "", testclaude.Config{StructuredTrailing: `{"type":"tool_use_summary","summary":"PRIVATE_MARKER","preceding_tool_use_ids":["serializer-only"]}` + "\n"}},
		{"slow-cleanup", "", "", testclaude.Config{StructuredAfterResultMillis: 50, StructuredTrailing: "{\"type\":\"keep_alive\"}\n"}},
		{"malformed-tail", "structured_protocol_invalid", "structured_json_decode_failed", testclaude.Config{StructuredAfterResultMillis: 20, StructuredTrailing: "{PRIVATE_MARKER}\n"}},
		{"bash-after-result", "structured_protocol_invalid", "structured_trailing_frame", testclaude.Config{StructuredTrailing: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","id":"PRIVATE_ID","input":{"command":"PRIVATE_MARKER"}}]}}` + "\n"}},
		{"held-pipe", "structured_termination_timeout", "structured_termination_timeout", testclaude.Config{StructuredHoldAfterResult: true, IgnoreTermination: true, Child: true}},
		{"generation-no-output", "structured_generation_timeout", "structured_generation_timeout", testclaude.Config{Wait: true, Child: true}},
		{"serializer-retry", "", "", testclaude.Config{StructuredSerializerFailures: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, req, opt := structuredClient(t)
			c.config.ToolBridge.TimeoutMillis = 300
			c.terminationGrace = 150 * time.Millisecond
			cfg := tc.config
			cfg.Subscription = "team"
			f.Set(t, cfg)
			var tools atomic.Int32
			opt.Tools = func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
				tools.Add(1)
				return nil, errors.New("unexpected action")
			}
			r, err := c.Respond(t.Context(), req, opt)
			if tc.code == "" {
				if err != nil || len(r.Output) != 1 {
					t.Fatal("validated EOF/termination failed", err)
				}
			} else {
				var e *Error
				if !errors.As(err, &e) || e.Code != tc.code || e.StructuredStage() != tc.stage {
					t.Fatal("wrong safe lifecycle failure", err)
				}
				if len(r.Output) != 0 || strings.Contains(err.Error(), "PRIVATE_MARKER") || strings.Contains(err.Error(), "PRIVATE_ID") {
					t.Fatal("failure exposed provider data or response")
				}
			}
			if tools.Load() != 0 {
				t.Fatal("unverified candidate reached the tool boundary")
			}
			for _, call := range f.Calls(t) {
				for _, pid := range []int{call.PID, call.ChildPID} {
					if pid > 0 {
						waitForProcessGone(t, pid)
					}
				}
			}
		})
	}
}

func TestStructuredOwnerCancelNeverAcceptsCandidate(t *testing.T) {
	c, f, req, opt := structuredClient(t)
	c.terminationGrace = 200 * time.Millisecond
	f.Set(t, testclaude.Config{Subscription: "team", StructuredHoldAfterResult: true, IgnoreTermination: true, Child: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		r, err := c.Respond(ctx, req, opt)
		if len(r.Output) != 0 {
			done <- errors.New("canceled candidate accepted")
			return
		}
		done <- err
	}()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(f.Calls(t)) == 0 {
		select {
		case <-deadline.C:
			t.Fatal("fake process not started")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("owner cancellation not propagated", err)
	}
	for _, call := range f.Calls(t) {
		waitForProcessGone(t, call.PID)
		if call.ChildPID > 0 {
			waitForProcessGone(t, call.ChildPID)
		}
	}
}
