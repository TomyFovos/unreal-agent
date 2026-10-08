package analysis

import (
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"strings"
	"testing"
	"time"
)

func TestUnknownProviderUsageIsNotMeasuredZeroInViewsOrExport(t *testing.T) {
	a := New("claude")
	a.Apply(host.HistoryItem{Data: session.Turn{ID: "turn"}})
	a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Usage: llm.Usage{OutputTokens: 12, Unknown: []llm.UsageField{llm.UsageInput, llm.UsageReasoning, llm.UsageCachedInput, llm.UsageCacheWriteInput}}}}})
	r := a.Snapshot(time.Now(), true, false, nil, nil)
	if len(r.Context) != 0 || !r.Usage.Partial || !r.Usage.Known {
		t.Fatal("unknown input created a context measurement")
	}
	for _, view := range []string{"Overview", "Usage", "Turns"} {
		s := strings.Join(Lines(r, view, true), "\n")
		if !strings.Contains(s, "unknown") || strings.Contains(s, "reasoning     ~0") || strings.Contains(s, "input         ~0") {
			t.Fatal("unknown values rendered as zeros", view, s)
		}
	}
	md, e := Encode(r, "Markdown")
	if e != nil || !strings.Contains(string(md), "reasoning     unknown") {
		t.Fatal("export invented reasoning zero", e)
	}
	data, e := Encode(r, "JSON")
	if e != nil || !strings.Contains(string(data), `"Unknown"`) || !strings.Contains(string(data), `"reasoning"`) {
		t.Fatal("JSON export lost missing-field metadata", e)
	}
}
