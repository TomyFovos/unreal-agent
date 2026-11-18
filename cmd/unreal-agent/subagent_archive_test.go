//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

type archiveCLIModel struct{}

func (archiveCLIModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, fmt.Errorf("fixture must not perform inference")
}

func TestSubagentShowWaitAndHistoryOnArchivedHandle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	children := testsession.WriteSettledChildren(t, dir, 3, 1024)
	h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveCLIModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err = h.Resume(t.Context(), host.Options{ID: "main"}); err != nil {
		t.Fatal(err)
	}
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		complete := true
		for _, id := range children {
			entry, err := store.ArchiveIndex(id)
			complete = complete && err == nil && !entry.Pending
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Host never archived settled children")
		case <-time.After(10 * time.Millisecond):
		}
	}
	socket := filepath.Join(privateCLIDirectory(t), "archive.sock")
	serverCtx, stopServer := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- gateway.ListenAndServe(serverCtx, socket, gateway.Config{Host: h, Policy: permission.Unrestricted(), Extension: viewer.NewExtensionHandler(h, dir)})
	}()
	defer func() { stopServer(); <-done }()
	client := gateway.NewClient(socket)
	defer client.Close()
	for {
		if _, err := client.Methods(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Setenv(subagentSocketEnv, socket)
	t.Setenv(subagentSessionEnv, "main")
	for _, args := range [][]string{{"show", "spawn-0000", "--task"}, {"wait", "spawn-0000", "--timeout", "1s"}, {"list", "--all"}} {
		var out bytes.Buffer
		if err := runSubagentCLI(ctx, args, strings.NewReader(""), &out); err != nil {
			t.Fatal(args, err)
		}
		for _, want := range []string{"spawn-0000", "completed", "synthetic report"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v lost %q: %s", args, want, out.String())
			}
		}
	}
	remote := viewer.Remote{ParentID: "main", Parent: client, Call: client.Extension}
	for range 2 { // First observation and cached subsequent observation.
		view, err := remote.Inspect(ctx, children[0], 0, 256)
		if err != nil || len(view.History.Items) == 0 || view.Session.Session.ID != children[0] {
			t.Fatal("archived history/cache lookup failed", view, err)
		}
	}
	for _, args := range [][]string{{"review", "spawn-0000", "reject", "--reason", "needs a focused fix"}, {"review", "spawn-0000", "accept"}} {
		var out bytes.Buffer
		if err := runSubagentCLI(ctx, args, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), "Recorded") {
			t.Fatal(args, out.String(), err)
		}
	}
	owner, err := h.Attach("main")
	if err != nil {
		t.Fatal(err)
	}
	v, err := owner.Inspect(0, 256)
	if err != nil {
		t.Fatal(err)
	}
	var reviews []sessionstore.ChildReview
	for _, item := range v.History.Items {
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.ChildReview != nil {
			reviews = append(reviews, *r.ChildReview)
		}
	}
	if len(reviews) != 2 || reviews[0].Decision != "reject" || reviews[0].Reason != "needs a focused fix" || reviews[1].Decision != "accept" || reviews[1].Reason != "" {
		t.Fatal("missing optional parent review", reviews)
	}
	first, err := owner.RecordChildReview(ctx, owner.Generation, reviews[1])
	if err != nil {
		t.Fatal(err)
	}
	again, err := owner.RecordChildReview(ctx, owner.Generation, reviews[1])
	if err != nil || first != again {
		t.Fatal("retry was not idempotent", first, again, err)
	}
	changed := reviews[1]
	changed.Decision = "reject"
	if _, err := owner.RecordChildReview(ctx, owner.Generation, changed); !errors.Is(err, host.ErrConflict) {
		t.Fatal("changed retry accepted", err)
	}
	if _, err := owner.RecordChildReview(ctx, "stale", reviews[1]); !errors.Is(err, host.ErrStaleGeneration) {
		t.Fatal("stale parent accepted", err)
	}
	changed = reviews[1]
	changed.InputID = "not-owned"
	changed.ChildID = "unrelated"
	if _, err := owner.RecordChildReview(ctx, owner.Generation, changed); err == nil {
		t.Fatal("review crossed parent boundary")
	}
	for _, action := range []string{"reject", "accept"} {
		var out bytes.Buffer
		if err := runContextCLI(ctx, []string{"state", action, "spawn-0000"}, strings.NewReader(""), &out); err != nil {
			t.Fatal(action, err)
		}
		var review contextstate.Result
		if err := json.Unmarshal(out.Bytes(), &review); err != nil {
			t.Fatal(err)
		}
		v, err := owner.Inspect(0, 256)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		var reviewLocator record.Locator
		for _, item := range v.History.Items {
			if r, ok := item.Data.(sessionstore.HostRecord); ok && r.ChildReview != nil {
				count++
				if r.ChildReview.Decision == action && r.ChildReview.Reason == "" {
					reviewLocator = record.Item(string(owner.ID), uint64(item.Sequence))
				}
			}
		}
		wantCount := 3
		if action == "accept" {
			wantCount = 4
		}
		if count != wantCount || review.Locator != reviewLocator || reviewLocator == "" {
			t.Fatal("state review wrote a different record", action, count, review.Locator, reviewLocator)
		}
		state, err := owner.ContextState(ctx)
		if err != nil || state.Children.Processed.Count != 1 || len(state.Children.Current) != 2 {
			t.Fatal("review counted as another child", state.Children, err)
		}
		if _, ok := state.Children.Processed.Sources[reviewLocator]; !ok {
			t.Fatal("L2 did not reference the canonical review", reviewLocator)
		}
		tree, err := analysis.ReadTree(ctx, owner.ID, remote.InspectTree)
		if err != nil {
			t.Fatal(err)
		}
		report := tree.Snapshot(nil)
		for _, worker := range report.Workers {
			wantRejected := 0
			if action == "reject" {
				wantRejected = 1
			}
			if worker.Rework.Reviewed != 1 || worker.Rework.Rejected != wantRejected ||
				!worker.Rework.RejectRate.Known || worker.Rework.RejectRate.Value != float64(wantRejected) {
				t.Fatalf("/analyze reviewed-child denominator changed: %+v", worker.Rework)
			}
		}
		if len(report.Workers) != 1 {
			t.Fatal("analysis skipped reviewed worker", report.Workers)
		}
	}
}
