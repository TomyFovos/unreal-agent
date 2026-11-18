package subagent_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

var briefQuestions = []struct {
	text, rule string
	progress   bool
}{
	{"brief root", "Q1", false},
	{"brief write_scope", "Q2", false},
	{"brief tmpdir", "Q3", false},
	{"brief prohibitions", "Q4", false},
	{"brief report_format", "Q5", false},
	{"brief purpose", "Q6", false},
	{"brief root", "", true},
	{"brief outputs", "X1", false},
	{"brief no_such_field", "X1", false},
	{"brief root ", "Q7", false},
	{"brief root\nCan I change it?", "Q7", false},
	{"Can I use the report format instead of the acceptance criteria?", "Q7", false},
}

var fullSummary = strings.Repeat("measured evidence\n", 200) + "Next step: parent reviews the report."

func runBriefRoutingFixture(ctx context.Context, c sub.ChildConfig, r llm.Request, turn int) (llm.Response, error) {
	answers := []string{c.Brief.Root, c.Brief.WriteScope, c.Brief.TMPDIR, c.Brief.Prohibitions, c.Brief.ReportFormat, c.Brief.Fields.Purpose}
	if turn > 0 && turn <= len(answers) {
		var last string
		for _, item := range r.Input {
			if result, ok := item.Data.(llm.ToolResult); ok && len(result.Output) > 0 {
				last = result.Output[0].Value
			}
		}
		if last != answers[turn-1] {
			return llm.Response{}, fmt.Errorf("brief reply %d = %q, want %q", turn, last, answers[turn-1])
		}
	}
	if turn == len(answers)+1 || turn == len(briefQuestions) {
		stage := "answered"
		if turn == len(briefQuestions) {
			stage = "forwarded"
		}
		if err := os.WriteFile(filepath.Join(c.Workspace, stage), nil, 0600); err != nil {
			return llm.Response{}, err
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			_, err := os.Stat(filepath.Join(c.Workspace, "release-"+stage))
			if !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					return llm.Response{}, err
				}
				break
			}
			select {
			case <-ctx.Done():
				return llm.Response{}, ctx.Err()
			case <-tick.C:
			}
		}
	}
	if turn < len(briefQuestions) {
		q := briefQuestions[turn]
		args, _ := json.Marshal(map[string]any{"text": q.text, "progress": q.progress})
		return response("SendParent", string(args)), nil
	}
	args, _ := json.Marshal(map[string]any{
		"status": "completed", "summary": fullSummary, "changedFiles": []string{"report.json"},
		"tests": []string{"go test ./fixture: PASS"}, "blockers": []string{"one unverified item"},
	})
	return response("Finish", string(args)), nil
}

func TestDelegationRoutingUsesStoredBriefAndWakesOnlyForForwardedInputs(t *testing.T) {
	workspace, dir := t.TempDir(), t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := sub.Config{
		Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestChildServerHelper$"},
		Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1", "TMPDIR="+workspace),
		Templates: map[string]sub.Template{
			"worker": {Workspace: workspace, Runtime: []byte(`{}`), Policy: permission.Config{Tools: []string{"Finish", "SendParent"}}},
		},
	}
	var calls atomic.Int32
	var userSeen atomic.Bool
	model := adapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		calls.Add(1)
		for _, item := range r.Input {
			if msg, ok := item.Data.(llm.Message); ok && msg.Text == "user wake" {
				userSeen.Store(true)
			}
		}
		return llm.Response{}, nil
	})
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(cfg, model)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	parent, err := h.Create(t.Context(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	fields := sub.BriefFields{Title: "Routing", Purpose: "  verbatim\nwith trailing spaces  "}
	receipt, err := sub.StartChildWithBrief(t.Context(), parent, parent.Generation, "start-brief", "worker", "brief routing fixture", fields, cfg.Templates, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := sub.StartChildWithBrief(t.Context(), parent, parent.Generation, "start-brief", "worker", "brief routing fixture", fields, cfg.Templates, nil, nil)
	if err != nil || again != receipt {
		t.Fatal("retry did not reuse start", again, err)
	}
	changed := fields
	changed.Purpose = "other"
	if _, err = sub.StartChildWithBrief(t.Context(), parent, parent.Generation, "start-brief", "worker", "brief routing fixture", changed, cfg.Templates, nil, nil); !errors.Is(err, host.ErrConflict) {
		t.Fatal("changed brief reused an input ID", err)
	}
	waitForStage := func(stage string) {
		t.Helper()
		awaitBoundary(t, func() bool {
			_, err := os.Stat(filepath.Join(workspace, stage))
			return err == nil
		})
	}
	release := func(stage string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, "release-"+stage), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}

	waitForStage("answered")
	if calls.Load() != 0 {
		t.Fatal("brief replies, ready or progress woke the parent", calls.Load())
	}
	release("answered")
	waitForStage("forwarded")
	awaitBoundary(t, func() bool { return calls.Load() > 0 })
	payload, _ := json.Marshal("user wake")
	if _, err := parent.Submit(t.Context(), parent.Generation, inbox.Input{ID: "user", Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	awaitBoundary(t, userSeen.Load)
	release("forwarded")

	handle := sub.StartOperationID(parent.ID, "start-brief")
	op := awaitInstructionChild(t, parent, handle)
	if op.Status != operation.StatusCompleted {
		t.Fatal("routing child did not finish", op)
	}
	awaitBoundary(t, func() bool { return parent.HasChildReport(handle) })
	view, err := parent.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	type forwarding struct{ text, rule string }
	var forwarded []forwarding
	ended, quiet := 0, 0
	for _, item := range view.History.Items {
		switch value := item.Data.(type) {
		case inbox.Input:
			if peer, err := value.DecodePeerMessage(); err == nil {
				switch peer.Kind {
				case "message":
					forwarded = append(forwarded, forwarding{peer.Text, peer.RuleID})
				case "ready", "progress":
					quiet++
				case "ended":
					ended++
					want := "Child " + string(handle) + " ended: completed\nSummary: " + fullSummary +
						"\nChanged files: report.json\nTests: go test ./fixture: PASS\nBlockers: one unverified item"
					if !peer.ReportIncluded || peer.Text != want {
						t.Fatalf("completion report was changed or truncated: %q", peer.Text)
					}
				}
			}
		case sessionstore.HostRecord:
			if value.Kind == sessionstore.HostChildReview {
				t.Fatal("the harness accepted the child's report")
			}
		}
	}
	var wantForwarded []forwarding
	for _, q := range briefQuestions {
		if q.rule == "X1" || q.rule == "Q7" {
			wantForwarded = append(wantForwarded, forwarding{q.text, q.rule})
		}
	}
	if !slices.Equal(forwarded, wantForwarded) || quiet != 2 || ended != 1 {
		t.Fatalf("forwarded=%q want=%q quiet=%d ended=%d", forwarded, wantForwarded, quiet, ended)
	}

	childID := sub.ChildID(parent.ID, handle)
	child, err := sub.ReadChildSnapshot(t.Context(), dir, childID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	childState, err := store.ContextState(t.Context(), childID)
	if err != nil {
		t.Fatal(err)
	}
	var answeredRules []string
	for _, event := range childState.RecentEvents {
		if event.Kind != "routing_answered" {
			continue
		}
		answeredRules = append(answeredRules, event.RuleID)
		page, err := parent.RecordShow(t.Context(), event.Locator, 0)
		if err != nil || !strings.Contains(string(page.Bytes), `"rule_id":"`+event.RuleID+`"`) {
			t.Fatalf("routing event lost its exact receipt: %+v %v", event, err)
		}
	}
	if !slices.Equal(answeredRules, []string{"Q1", "Q2", "Q3", "Q4", "Q5", "Q6"}) {
		t.Fatalf("L2 automatic routing: %v", answeredRules)
	}
	for _, op := range child.View.Operations {
		plan, err := sub.DecodePlan(op)
		if err != nil || plan.Action != "parent" || plan.Progress {
			continue
		}
		state, err := operation.DecodeRemoteJobState(op)
		var result sub.ParentMessageReceipt
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Handle) == 0 {
			continue // Forwarding evidence belongs to the parent's committed inbox.
		}
		if err := json.Unmarshal(state.Handle, &result); err != nil || result.Answer == "" {
			t.Fatal("invalid brief answer receipt", op, err)
		}
		key := strings.TrimPrefix(plan.Text, "brief ")
		want := "session:parent/operation:" + string(handle) + "/brief/v1/" + key
		if result.Locator != want {
			t.Fatalf("locator=%q want=%q", result.Locator, want)
		}
	}
	tree, err := analysis.ReadTree(t.Context(), parent.ID, func(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
		if id == parent.ID {
			return parent.Inspect(after, limit)
		}
		v, err := sub.ReadChild(ctx, dir, id, after, limit)
		return v.View, err
	})
	if err != nil {
		t.Fatal(err)
	}
	got := tree.Snapshot(nil)
	want := []analysis.RoutingCount{
		{RuleID: "F1", Forwarded: 1}, {RuleID: "P1", Handled: 2},
		{RuleID: "Q1", Handled: 1}, {RuleID: "Q2", Handled: 1}, {RuleID: "Q3", Handled: 1},
		{RuleID: "Q4", Handled: 1}, {RuleID: "Q5", Handled: 1}, {RuleID: "Q6", Handled: 1},
		{RuleID: "Q7", Forwarded: 3}, {RuleID: "U1", Forwarded: 1}, {RuleID: "X1", Forwarded: 2},
	}
	if got.Partial || !slices.Equal(got.Routing, want) {
		t.Fatalf("routing counts=%+v partial=%v, want %+v", got.Routing, got.Partial, want)
	}
	lines := strings.Join(analysis.Lines(analysis.Report{Tree: got}, "Overview", false), "\n")
	for _, want := range []string{"measured counts (session tree)", "Q6  1 handled · 0 forwarded", "Estimated parent wakeups saved by brief replies · up to 6"} {
		if !strings.Contains(lines, want) {
			t.Errorf("analysis is missing %q\n%s", want, lines)
		}
	}
}
