//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type contextCLIModel struct{}

func (contextCLIModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{Stop: llm.StopComplete}, nil
}

func TestContextCLIRoundTripInReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	policy, err := permission.New(permission.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	h, err := host.New(ctx, host.Config{Directory: t.TempDir(), PermissionMaximum: policy, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
		return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: contextCLIModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(ctx, host.Options{ID: "state-cli", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(privateCLIDirectory(t), "host.sock")
	serverCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- gateway.ListenAndServe(serverCtx, socket, gateway.Config{Host: h, Policy: policy}) }()
	defer func() { stop(); <-done }()
	t.Setenv(subagentSocketEnv, socket)
	t.Setenv(subagentSessionEnv, string(s.ID))
	client := gateway.NewClient(socket)
	defer client.Close()
	for {
		if _, err := client.Version(ctx, s.ID); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	run := func(args ...string) string {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if err := runExecutableWithStreams(ctx, "unreal-agent", args, &out, &diagnostics); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, diagnostics.String())
		}
		return out.String()
	}
	user := func(id, text string) record.Locator {
		t.Helper()
		payload, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Submit(ctx, s.Generation, inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		return record.Input(string(s.ID), uint64(r.Sequence), record.UserMessage)
	}
	const rule = "Keep  the original spacing"
	original := rule + "\r\n" + strings.Repeat(" 日本語", 6000)
	source := user("first", original)
	if got := run("record", "show", string(source)); got != original {
		t.Fatalf("record show changed %d bytes to %d bytes", len(original), len(got))
	}
	type textPage struct {
		Text     string         `json:"text"`
		Continue record.Locator `json:"continue"`
	}
	var first textPage
	if err := json.Unmarshal([]byte(run("record", "show", string(source), "--page-bytes", "4")), &first); err != nil ||
		first.Text != original[:4] || first.Continue == "" || run("record", "show", string(first.Continue)) != original[4:] {
		t.Fatalf("record continuation: %+v %v", first, err)
	}
	matches := run("record", "find", rule, "--kind", "user")
	if !strings.Contains(matches, string(source)) || !strings.Contains(matches, rule) {
		t.Fatal("record find lost the original excerpt or locator", matches)
	}
	run("state", "decide", "Use a typed reply", "--id", "choice", "--why", "Durable evidence", "--basis", string(source), "--rejected", `[{"alternative":"plain text","reason":"loses provenance"}]`)
	run("state", "plan", "step", "--text", "Verify", "--owner", "self", "--position", "3", "--status", "doing", "--decision", "choice")
	run("state", "rule", "add", rule, "--id", "spacing", "--source", string(source))
	var question contextstate.Result
	if err := json.Unmarshal([]byte(run("state", "question", "Which spacing?\nUse the original?", "--id", "question")), &question); err != nil ||
		question.ID != "question" || question.Locator == "" {
		t.Fatalf("missing note receipt: %+v %v", question, err)
	}
	response := user("answer", "Use the original spacing.")
	var state contextstate.State
	if err := json.Unmarshal([]byte(run("state", "show")), &state); err != nil {
		t.Fatal(err)
	}
	decision, step := state.Decisions["choice"], state.Plan.Steps[0]
	if decision.Reason != "Durable evidence" || decision.Locator == "" || !slices.Equal(decision.Basis, []record.Locator{source}) ||
		len(decision.Rejected) != 1 || step.Position != 3 || step.Status != contextstate.PlanDoing ||
		state.Rules["spacing"].Locator != source || len(state.QuestionsToUser.Current) != 0 ||
		state.QuestionsToUser.Processed.Count != 1 || state.QuestionsToUser.Processed.Locator == "" {
		t.Fatalf("CLI state round trip lost a note: %+v %+v %+v", decision, step, state.QuestionsToUser)
	}
	if raw := run("record", "show", string(question.Locator)); !strings.Contains(raw, "Which spacing?") || !strings.Contains(raw, "Use the original?") {
		t.Fatal("manual question lost its full text")
	}
	run("state", "question", "answer", "question", "--source", string(response))
	run("state", "plan", "step", "--status", "done")
	var after contextstate.State
	if err := json.Unmarshal([]byte(run("state", "show")), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Plan.Steps) != 0 || after.Plan.Processed.Count != 1 ||
		len(after.QuestionsToUser.Current) != 0 || after.QuestionsToUser.Processed.Count != 1 || after.Waiting.Processed.Count != 1 {
		t.Fatalf("processed plan or question not settled: %+v", after)
	}
	var evidence []record.Locator
	if err := json.Unmarshal([]byte(run("record", "show", string(after.QuestionsToUser.Processed.Locator))), &evidence); err != nil {
		t.Fatal(err)
	}
	for _, source := range []record.Locator{question.Locator, response} {
		if !slices.Contains(evidence, source) {
			t.Fatal("answer link lost evidence", source)
		}
	}
	view, err := s.Inspect(0, 1)
	if err != nil || view.Permissions.Mode != permission.ReadOnly || len(view.Permissions.Pending) != 0 {
		t.Fatal("state commands changed or requested permissions", view.Permissions, err)
	}
}
