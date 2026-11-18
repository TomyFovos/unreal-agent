package subagent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

func TestDelegationBriefRendering(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	snapshot, err := projectinstructions.FromContent([]byte("Original workspace instruction body."))
	if err != nil {
		t.Fatal(err)
	}
	child := ChildConfig{
		Workspace: root, Runtime: []byte(`{"DisallowedTools":["edit"]}`), ProjectInstructions: &snapshot,
		Policy: permission.Config{FilesystemUnrestricted: true, NetworkUnrestricted: true, ProcessMode: permission.ProcessUnrestricted},
		Task:   "Use the existing fixture.\nProhibitions: Keep shared inputs intact.",
	}
	fields := BriefFields{
		Title: "Fixture", Purpose: "Select a fixture", Inputs: "data/fixture.json, lines 4–8",
		Outputs: "report.json", Verification: "go test ./fixture\nExpected: PASS", AcceptanceCriteria: "Exact totals and command output",
	}
	for _, tc := range []struct {
		name  string
		input BriefFields
		mode  permission.Mode
		tmp   string
		cwd   string
		want  []string
		empty int
	}{
		{"all", fields, permission.FullAccess, "/scratch/tmp", cwd, []string{
			"root: " + root + "\nProcess cwd: " + cwd, "write_scope: Full Access\nUnrestricted filesystem", "tmpdir: /scratch/tmp",
			"title: Fixture", "purpose: Select a fixture", "inputs: data/fixture.json, lines 4–8", "outputs: report.json",
			"verification: go test ./fixture\nExpected: PASS", "acceptance_criteria: Exact totals and command output",
		}, 0},
		{"partial", BriefFields{Title: "Fixture"}, permission.Ask, "", root, []string{
			"write_scope: Ask for Approval\nWrites require parent approval", "tmpdir: process default", "title: Fixture",
		}, 5},
		{"free text", BriefFields{}, permission.ReadOnly, "", root, []string{
			"write_scope: Read Only\nWrites denied in Read Only mode", "Read Only mode denies write operations.",
		}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBrief(child, tc.input, tc.cwd, tc.tmp, tc.mode)
			got := b.Render(child.Task)
			for _, want := range append(tc.want,
				"Disabled tools: edit",
				"Bound workspace instructions: "+filepath.Join(root, "AGENTS.md"),
				"Keep shared inputs intact.\n\nreport_format: Status, artifact paths, measured verification",
				"certainty_labels: Mark claims confirmed, estimated, or unknown and give their source.",
				"next_step: Name the next action and its owner, or write none.",
				"Task instructions:\n"+child.Task,
			) {
				if !strings.Contains(got, want) {
					t.Errorf("brief missing %q\n%s", want, got)
				}
			}
			if strings.Count(got, unspecified) != tc.empty || strings.Contains(got, snapshot.Content) || (tc.cwd == root && strings.Contains(got, "Process cwd:")) {
				t.Fatalf("unexpected projection\n%s", got)
			}
		})
	}
}

func TestDelegationBriefTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", "/inherited")
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{nil, "/inherited"},
		{[]string{"OTHER=value"}, ""},
		{[]string{"TMPDIR=/first", "TMPDIR=/last"}, "/last"},
		{[]string{"TMPDIR="}, ""},
	} {
		if got := childTMPDIR(tc.env); got != tc.want {
			t.Errorf("childTMPDIR(%q) = %q, want %q", tc.env, got, tc.want)
		}
	}
}
