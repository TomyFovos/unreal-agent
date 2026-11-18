package analysis

import (
	"context"
	"encoding/json/v2"
	"io/fs"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

func storeInspect(t *testing.T, dir string) InspectFunc {
	t.Helper()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
		snapshot, items, ops, err := store.ReadSnapshot(ctx, id)
		if err != nil {
			return host.View{}, err
		}
		start := min(int(after), len(items))
		end := min(start+limit, len(items))
		next := after
		if end > start {
			next = items[end-1].Sequence
		}
		return host.View{Session: snapshot, Operations: ops, History: host.ProjectHistoryPage(sessionstore.Page{Items: items[start:end], NextAfter: next, More: end < len(items)})}, nil
	}
}

func TestWorkerTierTreeReworkArchiveAndExports(t *testing.T) {
	dir := t.TempDir()
	fixture := testsession.WriteWorkerTierTree(t, dir)
	inspect := storeInspect(t, dir)
	tree, err := ReadTree(t.Context(), "main", inspect)
	if err != nil {
		t.Fatal(err)
	}
	r := tree.Snapshot(nil)
	if r.Partial || len(r.Sessions) != 8 || len(r.Layers) != 4 || len(r.Workers) != 2 || len(r.Tasks) != 5 {
		t.Fatalf("incomplete tree: %+v", r)
	}
	var flash, qwen WorkerSummary
	for _, w := range r.Workers {
		if w.Model == "deepseek-v4.1-flash" {
			flash = w
		} else if w.Model == "qwen3.8-flash" {
			qwen = w
		}
	}
	if flash.Rework.Normal != 2 || flash.Rework.Reviewed != 2 || flash.Rework.Rejected != 1 || flash.Rework.Reworked != 1 || flash.Rework.Redelegated != 1 || flash.Rework.Failed != 1 || flash.Rework.RejectRate.Value != .5 || flash.Rework.ReworkRate.Value != .5 {
		t.Fatalf("wrong flash denominator/counts: %+v", flash)
	}
	if qwen.Rework.Normal != 2 || qwen.Rework.Reviewed != 1 || qwen.Rework.Unreviewed != 1 || qwen.Rework.Canceled != 1 || qwen.Rework.ReworkRate.Value != 0 || qwen.TotalTokens.Unknown != 1 {
		t.Fatalf("counted unreviewed/canceled as reviewed normal: %+v", qwen)
	}
	if flash.Tasks != 2 || flash.Attempts != 3 || flash.WallTime.Value != 50 || flash.TimePerTask.Value != 25 || flash.Elapsed.Value != 60 || flash.ElapsedPerTask.Value != 30 || flash.TotalTokens.Value != 3400 || flash.TokensPerTask.Value != 1700 {
		t.Fatalf("lost retry cost: %+v", flash)
	}
	for _, g := range r.Layers {
		if g.Layer == "worker" && g.Model == "deepseek-v4.1-flash" {
			if g.Requests != 3 || g.Usage.Input != 2100 || g.Usage.Output != 1300 || g.Usage.CachedInput != 400 || g.OutputTPS.Value != 260 || g.OutputTPS.Measured != 3 || g.UsageLimitWait.Value != 2 || g.ChildWallTime.Value != 50 || g.FirstResponse.Value != 11 || !g.ListPriceUSD.Known {
				t.Fatalf("wrong measured metrics: %+v", g)
			}
		}
		if g.Layer == "worker" && g.Model == "qwen3.8-flash" {
			if g.OutputTPS.Value != 100 || g.OutputTPS.Derived != 1 || g.OutputTPS.Unknown != 1 || g.RequestDuration.Value != 5 || g.RequestDuration.Unknown != 1 || g.RecordedLayers != 2 || g.DerivedLayers != 1 {
				t.Fatalf("missing timing treated as zero/all tokens divided by known time: %+v", g)
			}
		}
		if g.Layer == "lead" && (g.Model != "gpt-6.2-sol" || g.ListPriceUSD.Known || g.ListPriceUSD.Unknown != 1) {
			t.Fatalf("invented price for unknown model: %+v", g)
		}
	}
	for _, s := range r.Sessions {
		if s.ID == fixture.Workers["qwen-no-timing"] && (s.LayerSource != "derived" || s.Requests[0].Duration.Known || s.Requests[0].OutputTPS.Known) {
			t.Fatal("legacy layer/timing not labelled", s)
		}
	}
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []localfile.ArchiveEntry{{ChildID: fixture.Workers["qwen-archived"], ParentID: fixture.Lead, OperationID: "qwen-archived", Status: operation.StatusCompleted}, {ChildID: fixture.Lead, ParentID: "main", OperationID: "lead-start", Status: operation.StatusCompleted}} {
		if moved, err := store.ArchiveChild(t.Context(), a); err != nil || !moved {
			t.Fatal("archive failed", moved, err)
		}
	}
	after, err := ReadTree(t.Context(), "main", inspect)
	if err != nil {
		t.Fatal(err)
	}
	ar := after.Snapshot(nil)
	ar.ObservedAt = r.ObservedAt
	if !reflect.DeepEqual(r, ar) {
		t.Fatal("archive changed layer/rework statistics")
	}
	report := Report{Version: 1, Session: "main", Tree: r}
	for _, format := range []string{"json", "markdown"} {
		data, err := Encode(report, format)
		if err != nil || strings.Contains(string(data), "sensitive") || !strings.Contains(string(data), "deepseek-v4.1-flash") {
			t.Fatal("unsafe/incomplete export", format, err, string(data))
		}
	}
	t.Log("/analyze Layers fixture:\n" + strings.Join(Lines(report, "Layers", true), "\n"))
}

func TestWorkerTierMissingDescendantKeepsPartialCosts(t *testing.T) {
	dir := t.TempDir()
	f := testsession.WriteWorkerTierTree(t, dir)
	inspect := storeInspect(t, dir)
	tree, err := ReadTree(t.Context(), "main", func(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
		if id == f.Workers["qwen-no-timing"] {
			return host.View{}, fs.ErrNotExist
		}
		return inspect(ctx, id, after, limit)
	})
	if err != nil {
		t.Fatal(err)
	}
	r := tree.Snapshot(nil)
	if !r.Partial || len(r.Sessions) != 8 {
		t.Fatal("missing descendant silently omitted", r)
	}
	for _, g := range r.Layers {
		if g.Model == "qwen3.8-flash" && (g.Unobserved != 1 || g.ListPriceUSD.Unknown != 2 || g.RequestDuration.Unknown != 1 || !g.Usage.Partial) {
			t.Fatal("partial cost/volume presented as complete", g)
		}
	}
}

func TestWorkerTierCompactionVolumeAndLayerSources(t *testing.T) {
	at := time.Unix(1000, 0)
	a := New("main")
	a.Created(at)
	choice := &sessionstore.RuntimeSelection{Provider: "opencode-go", Model: "qwen3.8-flash", Effort: llm.ReasoningEffortMax}
	a.active = choice
	a.Apply(host.HistoryItem{RecordedAt: at.Add(5 * time.Second), Data: sessionstore.HostRecord{Kind: sessionstore.HostContext, Context: &sessionstore.ContextRecord{Conversation: &llm.ConversationRecord{Phase: "compact", StartedAt: at, EndedAt: at.Add(5 * time.Second), Usage: &llm.Usage{InputTokens: 200, OutputTokens: 20}}}}})
	collector := New("collector-child")
	collector.Created(at)
	collector.metrics.selection = choice
	unknown := New("custom-child")
	unknown.Created(at)
	unknown.metrics.selection = choice
	a.metrics.starts["collector"] = childStart{activity: llm.ActivityRecord{Layer: "collector", StartedAt: at}}
	tree := &Tree{nodes: []*treeNode{{acc: a, observed: true}, {acc: collector, parent: "main", handle: "collector", template: "renamed-template", observed: true}, {acc: unknown, parent: "main", handle: "custom", template: "custom", observed: true}}}
	r := tree.Snapshot(nil)
	if r.Sessions[0].Requests[0].Kind != "compaction" || r.Sessions[0].Requests[0].Usage.Input != 200 || r.Sessions[0].FirstResponse.Known {
		t.Fatal("compaction hidden or mistaken for first response", r.Sessions[0])
	}
	if r.Sessions[1].Layer != "collector" || r.Sessions[1].LayerSource != "measured" || r.Sessions[2].Layer != "unknown" || r.Sessions[2].LayerSource != "derived" {
		t.Fatal("recorded layer reinterpreted / custom guessed", r.Sessions)
	}
}

func TestWorkerTierUnknownWaitAndNoReviewDenominator(t *testing.T) {
	m := newSessionMetrics("worker")
	at := time.Unix(1000, 0)
	m.apply(host.HistoryItem{RecordedAt: at, Data: sessionstore.HostRecord{Activity: &llm.ActivityRecord{Kind: "usage_limit_wait", StartedAt: at, RetryAt: at.Add(time.Hour)}}}, nil)
	if wait := m.waitMetric(true); wait.Known || wait.Unknown != 1 {
		t.Fatal("planned retry mistaken for measured wait", wait)
	}
	m.apply(host.HistoryItem{RecordedAt: at.Add(10 * time.Second), Data: sessionstore.HostRecord{Activity: &llm.ActivityRecord{Kind: "usage_limit_wait"}}}, nil)
	if wait := m.waitMetric(true); !wait.Known || wait.Value != 10 || wait.Derived != 1 {
		t.Fatal("legacy activity timestamp not derived", wait)
	}
	r := TreeReport{Sessions: []SessionMetric{{ID: "a", ParentID: "p", Layer: "worker", LayerSource: "derived", ModelKey: ModelKey{Model: "m"}, Observed: true, StartedAt: at, EndedAt: at.Add(time.Second), Finish: "completed", title: "same"}, {ID: "b", ParentID: "p", Layer: "worker", ModelKey: ModelKey{Model: "m"}, Observed: true, StartedAt: at.Add(2 * time.Second), EndedAt: at.Add(3 * time.Second), Finish: "completed", title: "same"}}}
	r.buildTasks()
	r.aggregate()
	if w := r.Workers[0]; w.Rework.Redelegated != 1 || w.Rework.Unreviewed != 2 || w.Rework.Reviewed != 0 || w.Rework.ReworkRate.Known {
		t.Fatal("inference manufactured an explicit review denominator", w)
	}
}

func TestWorkerTierModelSwitchAndMixedRework(t *testing.T) {
	a := New("root")
	at := time.Unix(1000, 0)
	a.Created(at)
	a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"Provider":{"provider":"opencode-go","model":{"id":"deepseek-v4.1-flash"}},"ReasoningEffort":"max"}`)}})
	a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{Response: llm.Response{Usage: llm.Usage{InputTokens: 10, OutputTokens: 20}}}})
	a.Apply(host.HistoryItem{Data: sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &sessionstore.RuntimeSelection{Provider: "other", Model: "changed", Effort: llm.ReasoningEffortLow}}})
	a.Apply(host.HistoryItem{Data: sessionstore.ModelResponse{Response: llm.Response{Usage: llm.Usage{InputTokens: 30, OutputTokens: 40}}}})
	r := (&Tree{nodes: []*treeNode{{acc: a, observed: true}}}).Snapshot(nil)
	if len(r.Layers) != 2 || r.Layers[0].Requests+r.Layers[1].Requests != 2 {
		t.Fatal("model switch reassigned earlier requests", r.Layers)
	}
	s := []SessionMetric{{ID: "a", ParentID: "p", Layer: "worker", ModelKey: ModelKey{Model: "fast"}, Observed: true, StartedAt: at, EndedAt: at.Add(time.Second), Finish: "completed", Review: "reject", RejectionEvents: 1, title: "task"}, {ID: "b", ParentID: "p", Layer: "worker", ModelKey: ModelKey{Model: "slow"}, Observed: true, StartedAt: at.Add(2 * time.Second), EndedAt: at.Add(3 * time.Second), Finish: "completed", Review: "accept", title: "task"}}
	r = &TreeReport{Sessions: s}
	r.buildTasks()
	r.aggregate()
	if len(r.Tasks) != 1 || !r.Tasks[0].MixedModels || r.Tasks[0].Model != "fast" {
		t.Fatal("mixed-model task split or attributed to successful retry", r.Tasks)
	}
}

func TestWorkerTierPricesUnknownAndPeak(t *testing.T) {
	u := llm.Usage{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 600}
	k := ModelKey{Provider: "opencode-go", Model: "deepseek-v4.1-flash"}
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	base, ok := listPrice(k, u, at)
	if !ok || math.Abs(base-.0004806) > 1e-10 {
		t.Fatal("incorrect list price", base, ok)
	}
	peak, ok := listPrice(k, u, at.Add(-10*time.Hour))
	if !ok || peak != 2*base {
		t.Fatal("lost advertised multiplier", peak, base)
	}
	u.Unknown = []llm.UsageField{llm.UsageCachedInput}
	if _, ok := listPrice(k, u, at); ok {
		t.Fatal("priced unknown cached reads")
	}
	data, err := json.Marshal(Report{Tree: &TreeReport{Layers: []LayerModel{{OutputTPS: Metric{Unknown: 1}}}}})
	if err != nil || !strings.Contains(string(data), `"Known":false`) {
		t.Fatal("unknown lost in JSON", err, string(data))
	}
}
