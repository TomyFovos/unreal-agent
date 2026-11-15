package claudeagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestNativeToolsRecordStartBeforeResultOrBatch(t *testing.T) {
	for _, name := range []string{"Read", "Write", "Agent", "WebFetch"} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
			var activities []llm.ActivityRecord
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			round := newTurn(ctx, func() {}, llm.RequestOptions{RecordActivity: func(_ context.Context, a llm.ActivityRecord) error { activities = append(activities, a); return nil }}, &writer{w: io.Discard}, observed.NewResults(), &approver{}, t.TempDir(), "session")
			round.now = func() time.Time { return now }
			start := func(id string) {
				data, _ := json.Marshal(map[string]any{"id": "m-" + id, "content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]string{"private": "must-not-be-recorded"}}}})
				round.handle(frame{Type: "assistant", Message: data})
			}
			start("native-1")
			start("native-1")
			if len(activities) != 1 || activities[0].Kind != "native_tool" || activities[0].Tool != name || activities[0].StartedAt != now || !activities[0].EndedAt.IsZero() {
				t.Fatal("tool start missing while result is pending", activities)
			}
			now = now.Add(3 * time.Second)
			round.handle(frame{Type: "user", Message: []byte(`{"content":[{"type":"tool_result","tool_use_id":"native-1","content":"private result"}]}`)})
			round.endNativeTool("native-1", "post_tool_batch")
			if len(activities) != 2 || activities[1].EndedAt.Sub(activities[1].StartedAt) != 3*time.Second {
				t.Fatal("tool completion duplicated or lost", activities)
			}
			// A missing result remains unknown when the owning process ends.
			start("native-2")
			now = now.Add(time.Second)
			cancel(errors.New("test interrupted"))
			round.endNativeToolCalls()
			round.endNativeToolCalls()
			if len(activities) != 4 || activities[3].Reason != "test interrupted" || activities[3].EndedAt.Sub(activities[3].StartedAt) != time.Second {
				t.Fatal(activities)
			}
			data, _ := json.Marshal(activities)
			if strings.Contains(string(data), "private") || strings.Contains(string(data), "must-not") {
				t.Fatal("activity leaked tool arguments or result")
			}
		})
	}
}
