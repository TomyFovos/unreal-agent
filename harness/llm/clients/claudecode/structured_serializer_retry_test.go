//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/testclaude"
)

func TestStructuredOfficialControlSerializerCorrection(t *testing.T) {
	for _, failures := range []int{1, 4, 5} {
		t.Run(string(rune('0'+failures)), func(t *testing.T) {
			c, f, req, opt := structuredClient(t)
			f.Set(t, testclaude.Config{Subscription: "team", StructuredSerializerFailures: failures})
			var actions atomic.Int32
			opt.Tools = func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
				actions.Add(1)
				return nil, nil
			}
			response, err := c.Respond(t.Context(), req, opt)
			if failures == 5 {
				requireCode(t, err, "structured_serializer_round_limit")
				requireStructuredStage(t, err, "structured_serializer_round_limit")
				if len(response.Output) != 0 {
					t.Fatal("failed serializer produced a public response")
				}
			} else if err != nil || len(response.Output) != 1 || response.Output[0].Type != llm.ItemMessage {
				t.Fatal("correction did not reach authoritative Final", err)
			}
			if actions.Load() != 0 || len(f.Calls(t)) != 1 {
				t.Fatal("serializer helper executed a host Action or restarted generation")
			}
			encoded, _ := json.Marshal(response)
			if strings.Contains(string(encoded), "private-") {
				t.Fatal("serializer private value leaked")
			}
		})
	}
	// Retry helpers may contain invalid action-shaped JSON. Only the accepted
	// result below reaches the existing host callback, exactly once.
	c, f, req, opt := structuredClient(t)
	f.Set(t, testclaude.Config{Subscription: "team", StructuredSerializerFailures: 1, BridgeSteps: []testclaude.BridgeStep{{Name: "read", Arguments: `{"path":"fixture.go"}`, Contains: "canonical file receipt"}}})
	var actions atomic.Int32
	opt.Tools = func(ctx context.Context, response llm.Response) ([]llm.ToolOutcome, error) {
		actions.Add(1)
		return bridgeReply(ctx, response)
	}
	response, err := c.Respond(t.Context(), req, opt)
	if err != nil || actions.Load() != 1 || len(f.Calls(t)) != 2 || len(response.Output) != 1 || response.Output[0].Type != llm.ItemMessage {
		t.Fatal("serializer correction duplicated/lost host Action", err, actions.Load())
	}
}

func TestStructuredLaunchRejectsUnboundedSerializerTurns(t *testing.T) {
	for _, value := range []string{"1", "0", "6", "999999"} {
		requireCode(t, validateStructuredLaunch(append(structuredArgs(), "--max-turns", value)), "isolation_contract_invalid")
	}
	requireCode(t, validateStructuredLaunch(append(structuredArgs(), "--max-turns", "5", "--max-turns", "5")), "isolation_contract_invalid")
	requireCode(t, validateStructuredLaunch(append(structuredArgs(), "--max-turns=5")), "isolation_contract_invalid")
}
