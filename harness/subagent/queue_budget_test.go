package subagent_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func ledgerParent(t *testing.T, dir string, mode host.Mode, cap int, model adapter) (*host.Host, *host.Session, map[string]sub.Template) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := sub.Template{Workspace: dir, Runtime: jsontext.Value(`{"Provider":{"provider":"fake","model":{"id":"budget-model"}},"ReasoningEffort":"high"}`),
		Policy: permission.Config{Tools: []string{"Finish", "SendParent"}, ProcessMode: permission.ProcessUnrestricted, FilesystemUnrestricted: true, NetworkUnrestricted: true}}
	lead := worker
	lead.Policy.Tools = append([]string{"SubagentStart"}, worker.Policy.Tools...)
	lead.Subagents = map[string]sub.Template{"worker": worker}
	templates := map[string]sub.Template{"default": worker, "lead": lead}
	c := sub.Config{Directory: dir, Binary: exe, Arguments: []string{"-test.run=^TestChildServerHelper$"}, Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1"),
		Templates: templates, MaxActiveChildren: cap}
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: factory(c, model)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	s, err := h.Open(t.Context(), host.Options{Mode: mode, ID: "root", Policy: permission.Unrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	return h, s, templates
}

func startLedgerChild(t *testing.T, p *host.Session, templates map[string]sub.Template, id, template, task string, budget int) operation.ID {
	t.Helper()
	if _, err := sub.StartChildWithBrief(t.Context(), p, p.Generation, inbox.ID(id), template, task, sub.BriefFields{Title: task, BudgetSteps: budget}, templates, nil, nil); err != nil {
		t.Fatal(err)
	}
	return sub.StartOperationID(p.ID, inbox.ID(id))
}

func waitLedger(t *testing.T, r *sub.UsageReader, root session.ID, ready func(sub.UsageReport) bool) sub.UsageReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		report, err := r.Read(ctx, root, time.Now(), time.Hour)
		if err == nil && ready(report) {
			return report
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ledger condition not met, Sessions %+v, error %v", report.Sessions, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func ledgerChild(report sub.UsageReport, id operation.ID) sub.SessionUsage {
	for _, row := range report.Sessions {
		if row.Handle == id {
			return row
		}
	}
	return sub.SessionUsage{}
}

func ledgerHandle(t *testing.T, s *host.Session, id operation.ID) sub.Handle {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		view, err := s.Inspect(0, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range view.Operations {
			if op.ID == id {
				if h, err := sub.DecodeHandle(op); err == nil {
					return h
				}
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no handle for %s", id)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func cancelLedgerChild(t *testing.T, p *host.Session, id operation.ID) {
	t.Helper()
	if _, err := p.CancelChildOperation(t.Context(), p.Generation, inbox.ID("cancel-"+string(id)), id); err != nil {
		t.Fatal(err)
	}
}

func TestChildQueueFIFOByTreeAndRestoresQueuedDescendants(t *testing.T) {
	for _, mode := range []string{"live", "restart", "writer-draining"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			idle := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
				return llm.Response{Stop: llm.StopComplete}, nil
			})
			h, p, templates := ledgerParent(t, dir, host.Create, 2, idle)
			r, err := sub.OpenUsageReader(dir)
			if err != nil {
				t.Fatal(err)
			}
			holder := startLedgerChild(t, p, templates, "holder", "default", "wait", 0)
			if !ledgerHandle(t, p, holder).Started {
				t.Fatal("holder did not reserve a slot")
			}
			lead := startLedgerChild(t, p, templates, "lead", "lead", "queue-lead", 0)
			leadID := sub.ChildID(p.ID, lead)
			tree := waitLedger(t, r, p.ID, func(tree sub.UsageReport) bool {
				for _, row := range tree.Sessions {
					if row.Parent == leadID && row.State == "queued" {
						return true
					}
				}
				return false
			})
			var worker operation.ID
			for _, row := range tree.Sessions {
				if row.Parent == leadID {
					worker = row.Handle
				}
			}
			after := startLedgerChild(t, p, templates, "after", "default", "finish", 0)
			if state := ledgerHandle(t, p, after); state.QueuedAhead == nil || *state.QueuedAhead != 1 || state.Started {
				t.Fatalf("expected the nested job ahead, got %+v", state)
			}
			cancel := startLedgerChild(t, p, templates, "cancel", "default", "finish", 0)
			if state := ledgerHandle(t, p, cancel); state.QueuedAhead == nil || *state.QueuedAhead != 2 {
				t.Fatalf("expected two queued jobs ahead, got %+v", state)
			}
			cancelLedgerChild(t, p, cancel)
			waitLedger(t, r, p.ID, func(tree sub.UsageReport) bool { return ledgerChild(tree, cancel).State == "canceled" })
			if mode == "live" {
				cancelLedgerChild(t, p, holder)
			} else {
				if err := h.Close(); err != nil {
					t.Fatal(err)
				}
				release := func() {}
				if mode == "writer-draining" {
					store, err := localfile.OpenExisting(dir)
					if err != nil {
						t.Fatal(err)
					}
					writer, err := store.AcquireWriter(leadID)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = writer.Close() })
					release = func() {
						// An inbox receipt proves restoration ran while
						// the old executor still owned the child.
						payload, _ := json.Marshal("resume parent")
						if _, err := p.Submit(t.Context(), p.Generation, inbox.Input{ID: "handover", Kind: inbox.InputExternal, Payload: payload}); err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
				_, p, _ = ledgerParent(t, dir, host.Resume, 2, idle)
				release()
			}
			waitLedger(t, r, p.ID, func(tree sub.UsageReport) bool {
				return ledgerChild(tree, worker).State == "awaiting" && ledgerChild(tree, after).State == "queued"
			})
			if err := os.WriteFile(filepath.Join(dir, "release-child"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitLedger(t, r, p.ID, func(tree sub.UsageReport) bool {
				return ledgerChild(tree, worker).State == "completed" && ledgerChild(tree, after).State == "completed"
			})
			if _, err := os.Stat(filepath.Join(dir, string(sub.ChildID(p.ID, cancel))+".session.jsonl")); !os.IsNotExist(err) {
				t.Fatal("canceled queued child launched", err)
			}
		})
	}
}

func budgetInputs(t *testing.T, s *host.Session) map[string]inbox.PeerMessage {
	t.Helper()
	view, err := s.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]inbox.PeerMessage)
	for _, row := range view.History.Items {
		if input, ok := row.Data.(inbox.Input); ok && strings.HasPrefix(string(input.ID), "budget-notice:") {
			if _, ok := found[string(input.ID)]; ok {
				t.Fatalf("duplicate notice %s", input.ID)
			}
			peer, err := input.DecodePeerMessage()
			if err != nil {
				t.Fatal(err)
			}
			found[string(input.ID)] = peer
		}
	}
	return found
}

func TestBudgetNoticesWakeAt100And150AndSurviveRestore(t *testing.T) {
	dir := t.TempDir()
	var wakes atomic.Uint32
	idle := adapter(func(_ context.Context, req llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		for _, row := range req.Input {
			if message, ok := row.Data.(llm.Message); ok {
				if strings.Contains(message.Text, "5 of 5 budgeted model calls") {
					wakes.Or(1)
				}
				if strings.Contains(message.Text, "8 of 5 budgeted model calls") {
					wakes.Or(2)
				}
			}
		}
		return llm.Response{Stop: llm.StopComplete}, nil
	})
	h, p, templates := ledgerParent(t, dir, host.Create, 0, idle)
	start := startLedgerChild(t, p, templates, "budget", "default", "budget-steps", 5)
	stepFile := filepath.Join(dir, "allowed-step")
	release := func(n int) {
		t.Helper()
		if err := os.WriteFile(stepFile, []byte(fmt.Sprint(n)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := sub.OpenUsageReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	release(4)
	waitLedger(t, r, p.ID, func(tree sub.UsageReport) bool { return ledgerChild(tree, start).Own.Calls == 4 })
	deadline := time.Now().Add(5 * time.Second)
	for {
		child, err := sub.ReadChildSnapshot(t.Context(), dir, sub.ChildID(p.ID, start))
		found := false
		if err == nil {
			for _, item := range child.View.History.Items {
				if input, ok := item.Data.(inbox.Input); ok && strings.HasSuffix(string(input.ID), ":80") {
					peer, _ := input.DecodePeerMessage()
					found = strings.Contains(peer.Text, "Tell your parent before exceeding")
				}
			}
		}
		if found {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("80% notice did not reach child", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(budgetInputs(t, p)) != 0 || wakes.Load() != 0 {
		t.Fatal("80% notice woke parent")
	}
	waitNotice := func(half int, text string) {
		t.Helper()
		id := fmt.Sprintf("budget-notice:%s:budget:%d", start, half)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if m, ok := budgetInputs(t, p)[id]; ok {
				if m.Kind != "budget" || !strings.Contains(m.Text, text) {
					t.Fatalf("wrong notice: %+v", m)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("missing notice %s", id)
	}
	waitWake := func(mask uint32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for wakes.Load()&mask == 0 {
			if !time.Now().Before(deadline) {
				t.Fatalf("budget notice %d did not reach parent model", mask)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	release(5)
	waitNotice(2, "5 of 5 budgeted model calls (fake 5)")
	waitWake(1)
	release(8)
	waitNotice(3, "8 of 5 budgeted model calls (fake 8)")
	waitWake(2)
	waitLedger(t, r, p.ID, func(tree sub.UsageReport) bool { return ledgerChild(tree, start).State == "completed" })
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	_, p, _ = ledgerParent(t, dir, host.Resume, 0, idle)
	if got := budgetInputs(t, p); len(got) != 2 {
		t.Fatalf("notices were lost or repeated: %+v", got)
	}
}

func TestBudgetlessMilestoneStaysQuietUntilNextInput(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	seen := make(chan bool, 1)
	idle := adapter(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			var progress bool
			for _, item := range r.Input {
				if msg, ok := item.Data.(llm.Message); ok {
					peer, _ := modelPeer(msg.Text)
					progress = progress || peer.Kind == "progress" && strings.Contains(peer.Text, "reached 250 model calls")
				}
			}
			seen <- progress
		}
		return llm.Response{Stop: llm.StopComplete}, nil
	})
	_, p, templates := ledgerParent(t, dir, host.Create, 0, idle)
	start := startLedgerChild(t, p, templates, "quiet", "default", "quiet-milestone", 0)
	id := "budget-notice:" + string(start) + ":milestone:250"
	deadline := time.Now().Add(6 * time.Second)
	for !p.HasCommittedInput(inbox.ID(id)) {
		if !time.Now().Before(deadline) {
			t.Fatal("quiet milestone did not commit")
		}
		time.Sleep(15 * time.Millisecond)
	}
	if got := budgetInputs(t, p)[id]; got.Kind != "progress" || calls.Load() != 0 {
		t.Fatalf("milestone woke the idle parent: %+v, calls %d", got, calls.Load())
	}
	data, _ := json.Marshal("keep going")
	if _, err := p.Submit(t.Context(), p.Generation, inbox.Input{ID: "wake", Kind: inbox.InputExternal, Payload: data}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if !got {
			t.Fatal("next request omitted quiet milestone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request after user input")
	}
}
