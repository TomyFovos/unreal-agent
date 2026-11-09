package claudeagent

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

func TestCompactHooksRegisteredAndSummaryNotPersisted(t *testing.T) {
	h := newHarness(t, "compact_hooks")
	var activities []llm.ActivityRecord
	r, err := h.client.Respond(t.Context(), llm.Request{Input: []llm.Item{user("continue")}}, llm.RequestOptions{
		Tools:          h.tools,
		RecordActivity: func(_ context.Context, r llm.ActivityRecord) error { activities = append(activities, r); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Output) != 1 || !strings.Contains(r.Output[0].Data.(llm.Message).Text, "unreal-agent subagent list") {
		t.Fatal(r)
	}
	var summaries []llm.ActivityRecord
	for _, a := range activities {
		if a.Kind == "claude_compact_summary" {
			summaries = append(summaries, a)
		}
	}
	data, _ := json.Marshal(activities)
	if len(summaries) != 1 || summaries[0].Reason != "bytes=15" || bytes.Contains(data, []byte("private summary")) {
		t.Fatal(string(data))
	}
}

func TestCompactContextReadsCurrentHostState(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "unreal-agent")
	writeCLI := func(lines string) {
		t.Helper()
		script := "#!/bin/sh\n[ \"$1 $2\" = 'subagent list' ] || exit 2\n[ \"$UNREAL_AGENT_SESSION\" = parent ] || exit 2\n[ \"$UNREAL_AGENT_SOCKET\" = /run/host.sock ] || exit 2\ncat <<'EOF'\n" + lines + "\nEOF\n"
		if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	controller := permission.NewController(t.Context(), nil, nil, 0)
	defer controller.Close()
	if err := controller.Follow(permission.FullAccess); err != nil {
		t.Fatal(err)
	}
	ctx := permission.WithController(t.Context(), controller)
	round := newTurn(ctx, func() {}, llm.RequestOptions{Identity: llm.RequestIdentity{SessionID: "parent"}}, &writer{w: &bytes.Buffer{}}, observed.NewResults(), &approver{}, dir, "s")
	round.subagents = SubagentCLI{Binary: cli, Socket: "/run/host.sock"}
	round.environ = append(os.Environ(), SubagentSocketEnv+"=/run/host.sock", SubagentSessionEnv+"=parent")
	for _, tc := range []struct {
		name, list string
		active     bool
	}{
		{"legacy", "h1 lead (gpt max) awaiting waiting for 1 approval(s)\nh2 worker-high completed secret old report\nh3 collector ready\nh4 worker failed secret failure\nh5 worker canceled", true},
		{"summary", "h1 lead (gpt max) awaiting waiting for 1 approval(s)\nh3 collector ready\nended: 41 completed, 3 failed, 2 canceled (subagent list --all)", true},
		{"summary_only", "ended: 41 completed, 3 failed, 2 canceled (subagent list --all)", false},
		{"legacy_ended_only", "h2 worker completed\nh4 worker failed\nh5 worker canceled", false},
		{"empty", "No children in this Session.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeCLI(tc.list)
			text := round.compactContext()
			if !strings.Contains(text, permission.FullAccess.Label()) || strings.Contains(text, cli+" subagent list") {
				t.Fatal("known list format must parse without fallback", text)
			}
			for _, want := range []string{"h1 lead awaiting", "h3 collector ready"} {
				if strings.Contains(text, want) != tc.active {
					t.Fatal(text)
				}
			}
			if strings.Contains(text, "None.") == tc.active {
				t.Fatal("wrong active child count", text)
			}
			for _, unwanted := range []string{"h2", "h4", "h5", "secret", "gpt max", "ended:", "completed", "failed", "canceled"} {
				if strings.Contains(text, unwanted) {
					t.Fatal("terminal children or reports leaked into compact context", text)
				}
			}
		})
	}
	if err := controller.Follow(permission.ReadOnly); err != nil {
		t.Fatal(err)
	}
	if text := round.compactContext(); !strings.Contains(text, permission.ReadOnly.Label()) {
		t.Fatal(text)
	}
	writeCLI(strings.Repeat("long-handle worker-high awaiting long report\n", 500))
	if text := round.compactContext(); len(text) > maxCompactStateBytes || !strings.Contains(text, cli+" subagent list") {
		t.Fatal("state must be bounded and point to full list", len(text))
	}
	if err := os.Remove(cli); err != nil {
		t.Fatal(err)
	}
	if text := round.compactContext(); !strings.Contains(text, cli+" subagent list") || strings.Contains(text, "long-handle") {
		t.Fatal(text)
	}
}
