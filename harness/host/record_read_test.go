package host

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestRecordCommandsMeasurePreviousWindowEvidence(t *testing.T) {
	ctx, dir := t.Context(), t.TempDir()
	store, old, loc := seedWindowHistory(t, dir)
	previous := []record.Locator{loc}
	checkHostRestore(t, store.AppendHostRecord(ctx, "window", sessionstore.HostRecord{Version: 1, Kind: sessionstore.HostContext,
		Context: &sessionstore.ContextRecord{Kind: sessionstore.ContextWindowBoundary, Window: &sessionstore.WindowBoundary{
			Tail: &sessionstore.WindowTail{Previous: &previous},
		}},
	}))
	h := newTestHost(t, dir)
	defer h.Close()
	s, err := h.Resume(ctx, Options{ID: "window", Policy: permission.Unrestricted()})
	checkHostRestore(t, err)
	command := func(action contextstate.Action, args any) (contextstate.Result, error) {
		raw, err := json.Marshal(args)
		checkHostRestore(t, err)
		return s.ContextCommand(ctx, "", "context-read", contextstate.Command{Action: action, Args: raw})
	}
	a, err := loc.Address()
	checkHostRestore(t, err)
	a.Range = &record.ByteRange{Start: 0, End: 8}
	page, err := command(contextstate.ShowRecord, contextstate.RecordSelection{Locator: a.Locator()})
	checkHostRestore(t, err)
	if got := string(page.Page.Bytes); got != old[:8] {
		t.Fatal("record show changed the selected evidence", got)
	}
	results, err := command(contextstate.FindRecord, contextstate.RecordSearch{Kind: record.UserMessage, Text: oldWindowInput})
	checkHostRestore(t, err)
	if len(results.Matches) != 1 || results.Matches[0].Locator != loc {
		t.Fatal("record find returned the wrong evidence", results.Matches)
	}
	if _, err := command(contextstate.ShowRecord, contextstate.RecordSelection{Locator: record.Item("window", 10000)}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("record show failure changed", err)
	}
	v, err := s.Inspect(0, 64)
	checkHostRestore(t, err)
	var reads []*sessionstore.RecordRead
	for _, item := range v.History.Items {
		if h, ok := item.Data.(sessionstore.HostRecord); ok && h.Context != nil && h.Context.RecordRead != nil {
			reads = append(reads, h.Context.RecordRead)
		}
	}
	if len(reads) != 3 {
		t.Fatalf("got %d record command observations", len(reads))
	}
	for i, want := range []struct {
		action string
		bytes  int64
	}{{"show", 8}, {"find", int64(len(oldWindowInput))}} {
		r := reads[i]
		if r.Action != want.action || r.Bytes == nil || *r.Bytes != want.bytes || r.PreviousTailHits == nil || *r.PreviousTailHits != 1 {
			t.Fatalf("%s observation did not preserve sizes or window membership: %+v", want.action, r)
		}
	}
	if reads[2].Bytes != nil || reads[2].PreviousTailHits != nil || reads[1].After != reads[0].After+1 {
		t.Fatalf("failed read invented a value, or reads lost their start cursor: %+v", reads)
	}
}

func TestRecordShowAndFindAcrossCheckpointAndArchive(t *testing.T) {
	ctx := t.Context()
	store, child := childArchiveFixture(t, operation.StatusCompleted, false, false, false)
	parentPath, err := store.ResolveSessionPath("parent")
	checkHostRestore(t, err)
	dir := filepath.Dir(parentPath)
	const user = "exact user\r\n原文\n "
	seq, err := store.AppendInputRecorded(ctx, "parent", input("user", user))
	checkHostRestore(t, err)
	userLocator := record.Input("parent", uint64(seq), record.UserMessage)
	itemLocator := record.Item("parent", uint64(seq))
	parentLog, err := os.ReadFile(parentPath)
	checkHostRestore(t, err)
	userLine := parentLog[bytes.LastIndexByte(parentLog[:len(parentLog)-1], '\n')+1:]

	const childTurn = session.TurnID("child-turn")
	checkHostRestore(t, store.AppendTurn(ctx, child, session.Turn{ID: childTurn, Type: session.TurnRegular}))
	call := llm.ToolCall{CallID: "call-1", Name: "Bash", Arguments: "{\n  \"command\": \"printf\"\n}"}
	checkHostRestore(t, store.AppendModelResponse(ctx, child, sessionstore.ModelResponse{TurnID: childTurn, Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemToolCall, Data: call},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "assistant original\n"}},
	}}}))
	callAddress := record.Address{Session: string(child), Sequence: 3, Kind: record.ToolCall}
	callJSON, err := json.Marshal(call)
	checkHostRestore(t, err)
	assistantAddress := callAddress
	assistantAddress.Kind, assistantAddress.Part = record.AssistantMessage, 1

	base := filepath.Join(dir, "operations", string(child))
	opDir := filepath.Join(base, "shell")
	checkHostRestore(t, os.MkdirAll(opDir, 0700))
	// The match crosses an I/O buffer boundary and a long line ends in CRLF.
	const needle = "capture needle"
	stdout := append(bytes.Repeat([]byte{0xff}, 32*1024-2), []byte(needle+"\r\nsecond\n")...)
	stderr := []byte{0x01, 0xfe, 0x0a}
	outPath, errPath := filepath.Join(opDir, "out"), filepath.Join(opDir, "err")
	checkHostRestore(t, os.WriteFile(outPath, stdout, 0600))
	checkHostRestore(t, os.WriteFile(errPath, stderr, 0600))
	shell := operation.Operation{ID: "shell", Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusReady, MaxOutputLength: 1}
	checkHostRestore(t, store.AppendToolCallStatus(ctx, child, sessionstore.ToolCallStatus{
		TurnID: childTurn, CallID: call.CallID, Status: tool.CallStatus{WaitingFor: []operation.ID{shell.ID}}, Operations: []operation.Operation{shell},
	}))
	shell.Status = operation.StatusCompleted
	shell.State, err = json.Marshal(operation.ShellState{
		OutPath: outPath, ErrPath: errPath, OutTruncated: true, ErrTruncated: true,
		Result: &operation.ShellResult{Out: "preview", Err: "preview", OutSize: int64(len(stdout)), ErrSize: int64(len(stderr))},
	})
	checkHostRestore(t, err)
	opLocator, err := store.SaveOperationRecorded(ctx, child, shell)
	checkHostRestore(t, err)
	outLocator := record.Output(string(child), "shell", record.Stdout)
	lineAddress, err := outLocator.Address()
	checkHostRestore(t, err)
	lineAddress.Line = 2

	finish := sessionstore.FinishRecord{Version: 1, ModelTurnID: childTurn, Result: sessionstore.FinishResult{
		Status: "completed", Summary: "Report\nexact summary", Tests: []string{"focused pass"}, ChangedFiles: []string{"target path"},
		Blockers: []string{"none"}, Next: "review exact file",
	}}
	checkHostRestore(t, store.AppendHostRecord(ctx, child, sessionstore.HostRecord{Version: 1, Kind: "finish", Finish: &finish}))
	report := record.Address{Session: string(child), Sequence: 5, Kind: record.ChildReport, Handle: "start"}
	reportLocator := report.Locator()
	childPath, err := store.ResolveSessionPath(child)
	checkHostRestore(t, err)
	childLog, err := os.ReadFile(childPath)
	checkHostRestore(t, err)
	address, err := opLocator.Address()
	checkHostRestore(t, err)
	opLine := bytes.SplitAfter(childLog[address.Offset:], []byte{'\n'})[0]
	reportLine := bytes.SplitAfter(childLog, []byte{'\n'})[6] // header and an intervening standalone Operation
	payload, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(child), Target: "parent", Handle: "start",
		Kind: "ended", Text: "forwarded report", ReportIncluded: true, ReportLocator: reportLocator})
	checkHostRestore(t, err)

	artifactPath := filepath.Join(t.TempDir(), "例 ? # sample.bin")
	artifactBytes := []byte("file exact\x00\xff\n")
	checkHostRestore(t, os.WriteFile(artifactPath, artifactBytes, 0600))
	artifactItem, err := store.CaptureArtifact(ctx, child, artifactPath)
	checkHostRestore(t, err)
	artifact := artifactItem.Data.(sessionstore.HostRecord).Artifact.Locator(child, artifactItem.Sequence)
	checkHostRestore(t, os.Remove(artifactPath))
	_, err = store.Create(ctx, "unrelated")
	checkHostRestore(t, err)
	checkHostRestore(t, store.AppendInput(ctx, "unrelated", input("outside", "unrelated record")))

	open := func() (*Host, *Session) {
		h := newTestHost(t, dir)
		s, err := h.Resume(ctx, Options{ID: "parent", Policy: permission.Unrestricted()})
		checkHostRestore(t, err)
		return h, s
	}
	h, s := open()
	read := func(s *Session, loc record.Locator, size int64) []byte {
		t.Helper()
		var result []byte
		for loc != "" {
			page, err := s.RecordShow(ctx, loc, size)
			checkHostRestore(t, err)
			if size != 0 && int64(len(page.Bytes)) > size {
				t.Fatalf("page has %d bytes with requested size %d", len(page.Bytes), size)
			}
			if page.Continue != "" && (page.Continue == loc || len(page.Bytes) == 0) {
				t.Fatal("continuation made no progress", loc)
			}
			result = append(result, page.Bytes...)
			loc = page.Continue
		}
		return result
	}
	assertRecord := func(s *Session, locator record.Locator, want []byte, size int64) {
		t.Helper()
		if got := read(s, locator, size); !bytes.Equal(got, want) {
			t.Fatalf("%s: got %d bytes, want %d", locator, len(got), len(want))
		}
	}
	verify := func(s *Session) {
		t.Helper()
		assertRecord(s, itemLocator, userLine, 0)
		assertRecord(s, userLocator, []byte(user), 2)
		assertRecord(s, callAddress.Locator(), callJSON, 0)
		assertRecord(s, assistantAddress.Locator(), []byte("assistant original\n"), 0)
		assertRecord(s, opLocator, opLine, 0)
		assertRecord(s, outLocator, stdout, 7001)
		assertRecord(s, record.Output(string(child), "shell", record.Stderr), stderr, 1)
		assertRecord(s, lineAddress.Locator(), []byte("second\n"), 3)
		assertRecord(s, reportLocator, reportLine, 0)
		for section, want := range map[string]string{
			"summary": finish.Result.Summary, "tests": `["focused pass"]`, "changed-files": `["target path"]`, "blockers": `["none"]`, "next": finish.Result.Next,
		} {
			address := report
			address.Section = section
			assertRecord(s, address.Locator(), []byte(want), 3)
		}
		assertRecord(s, artifact, artifactBytes, 5)
		hits, err := s.RecordFind(ctx, record.Query{Kind: record.OperationOutput, Handle: "start", Text: needle})
		checkHostRestore(t, err)
		if len(hits) != 1 || hits[0].Locator != outLocator || string(hits[0].Excerpt) != needle || hits[0].At.IsZero() {
			t.Fatalf("operation lookup failed: %+v", hits)
		}
		hits, err = s.RecordFind(ctx, record.Query{Kind: record.OperationOutput, Handle: "start", Text: needle, From: hits[0].At, Before: hits[0].At.Add(time.Nanosecond)})
		checkHostRestore(t, err)
		if len(hits) != 1 {
			t.Fatalf("inclusive start / exclusive end lookup failed: %+v", hits)
		}
		for _, q := range []record.Query{
			{Kind: record.OperationOutput, Handle: "wrong", Text: needle},
			{Kind: record.OperationOutput, Text: needle, Before: hits[0].At},
			{Kind: record.UserMessage, Text: "unrelated"},
		} {
			hits, err := s.RecordFind(ctx, q)
			checkHostRestore(t, err)
			if len(hits) != 0 {
				t.Fatalf("invalid match for %+v: %+v", q, hits)
			}
		}
		hits, err = s.RecordFind(ctx, record.Query{Kind: record.ChildReport, Handle: "start", Text: "Report\nexact"})
		checkHostRestore(t, err)
		address := report
		address.Section = "summary"
		if len(hits) != 1 || hits[0].Locator != address.Locator() || string(hits[0].Excerpt) != "Report\nexact" {
			t.Fatalf("report lookup failed: %+v", hits)
		}
		if _, err := s.RecordShow(ctx, record.Input("unrelated", 1, record.UserMessage), 0); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("unrelated Session lookup", err)
		}
	}
	verify(s)
	checkHostRestore(t, h.Close())
	if _, err := os.Stat(filepath.Join(dir, "parent.restore-checkpoint")); err != nil {
		t.Fatal("Host did not publish its restore checkpoint", err)
	}
	h, s = open()
	verify(s)
	receipt, err := s.SubmitPeer(timeout(t), s.Generation, string(child), "start", inbox.Input{ID: "ended", Kind: inbox.InputPeer, Payload: payload})
	checkHostRestore(t, err)
	checkHostRestore(t, h.Close())
	if _, err := store.ArchiveIndex(child); err != nil {
		t.Fatal("child was not archived", err)
	}
	if _, err := os.Stat(outPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("operation capture still uses the live path", err)
	}
	_, s = open()
	verify(s)
	assertRecord(s, receipt.Locator, []byte("forwarded report"), 0)
	view, err := s.Inspect(receipt.Sequence-1, 1)
	checkHostRestore(t, err)
	refs := s.RecordReferences(view.History.Items[0])
	if len(refs) != 2 || refs[0].Locator != receipt.Locator || refs[1].Locator != reportLocator {
		t.Fatalf("peer correlation lost on restore: %+v", refs)
	}
}

func TestRecordReadsUseCurrentPermission(t *testing.T) {
	ctx := t.Context()
	base, outside := t.TempDir(), t.TempDir()
	baseline, err := permission.New(permission.Config{ReadRoots: []string{base}})
	checkHostRestore(t, err)
	defer baseline.Close()
	h, err := New(ctx, Config{Directory: t.TempDir(), Build: factory(modelFunc(echo)), PermissionMaximum: permission.Unrestricted()})
	checkHostRestore(t, err)
	defer h.Close()
	s, err := h.Create(ctx, Options{ID: "permissions", Policy: baseline, Workspace: base, PermissionMode: permission.FullAccess})
	checkHostRestore(t, err)
	controller, err := s.PermissionControl(s.Generation)
	checkHostRestore(t, err)
	allowed, blocked := filepath.Join(base, "allowed"), filepath.Join(outside, "blocked")
	checkHostRestore(t, os.WriteFile(allowed, []byte("allowed needle"), 0600))
	checkHostRestore(t, os.WriteFile(blocked, []byte("blocked needle"), 0600))
	blockedLoc, err := s.RecordArtifact(ctx, blocked)
	checkHostRestore(t, err)
	for _, mode := range []permission.Mode{permission.ReadOnly, permission.Ask, permission.FullAccess} {
		checkHostRestore(t, controller.Follow(mode))
		allowedLoc, err := s.RecordArtifact(ctx, "allowed")
		checkHostRestore(t, err)
		page, err := s.RecordShow(ctx, allowedLoc, 0)
		if err != nil || string(page.Bytes) != "allowed needle" {
			t.Fatal(mode, page, err)
		}
		page, err = s.RecordShow(ctx, blockedLoc, 0)
		if mode == permission.FullAccess {
			if err != nil || string(page.Bytes) != "blocked needle" {
				t.Fatal(mode, page, err)
			}
		} else {
			if permission.Failure(err) == nil || len(page.Bytes) != 0 {
				t.Fatal(mode, page, err)
			}
			if _, err := s.RecordArtifact(ctx, blocked); permission.Failure(err) == nil {
				t.Fatal("captured a path ordinary reads would deny", err)
			}
		}
		results, err := s.RecordFind(ctx, record.Query{Kind: record.Artifact, Text: "blocked needle"})
		checkHostRestore(t, err)
		if (len(results) != 0) != (mode == permission.FullAccess) {
			t.Fatalf("mode %s leaked or hid a match: %+v", mode, results)
		}
	}
}

func TestRecordRemoteOutputIsOriginalOrExplicitlyUnavailable(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := localfile.New(dir)
	checkHostRestore(t, err)
	const sid = session.ID("remote")
	lease, err := store.AcquireWriter(sid)
	checkHostRestore(t, err)
	t.Cleanup(func() { _ = lease.Close() })
	_, err = store.Create(ctx, sid)
	checkHostRestore(t, err)
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: "fixture", Version: 1, Data: []byte("{}")})
	checkHostRestore(t, err)
	spec.MaxOutputLength = 5
	encoded, err := json.Marshal(spec)
	checkHostRestore(t, err)
	original := strings.Repeat("full result\n", 4) + "\xff"
	for _, id := range []string{"new", "legacy"} {
		payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.DispatchOperation,
			Parameters: inbox.OperationIntent{OperationID: id, ToolName: "fixture", Spec: encoded}})
		checkHostRestore(t, err)
		input := inbox.Input{ID: inbox.ID(id), Kind: inbox.InputControl, Payload: payload}
		checkHostRestore(t, store.AppendInput(ctx, sid, input))
		op, err := sessionstore.DecodeOperationIntent(input)
		checkHostRestore(t, err)
		state, err := operation.DecodeRemoteJobState(op)
		checkHostRestore(t, err)
		state.TerminalResult = original
		step, err := operation.UpdateRemoteJob(op, state, operation.StatusCompleted)
		checkHostRestore(t, err)
		if id == "legacy" {
			state, err := operation.DecodeRemoteJobState(*step.Operation)
			checkHostRestore(t, err)
			state.OriginalResult = nil
			step.Operation.State, err = json.Marshal(state)
			checkHostRestore(t, err)
		}
		checkHostRestore(t, store.SaveOperation(ctx, sid, *step.Operation))
	}
	// Resume with both terminal fixtures committed. A ready fixture written
	// during coordinator startup could be restored as executable work.
	checkHostRestore(t, lease.Close())
	h := newTestHost(t, dir)
	s, err := h.Resume(ctx, Options{ID: sid})
	checkHostRestore(t, err)
	for _, id := range []string{"new", "legacy"} {
		page, err := s.RecordShow(ctx, record.Output(string(s.ID), id, record.Result), 0)
		if id == "new" && (err != nil || string(page.Bytes) != original) ||
			id == "legacy" && (!errors.Is(err, record.ErrUnavailable) || len(page.Bytes) != 0) {
			t.Fatal(id, page, err)
		}
	}
}
