package contextbuilder

import (
	_ "embed"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

//go:embed prompts/project-instructions.md
var projectInstructionsPreambleFile string

var projectInstructionsPreamble = strings.TrimSpace(projectInstructionsPreambleFile)

func formatProjectInstructions(snapshot projectinstructions.Snapshot) string {
	content := strings.TrimSpace(strings.TrimPrefix(snapshot.Content, "\uFEFF"))
	if snapshot.SourceKind != projectinstructions.SourceWorkspaceAgents || content == "" {
		return ""
	}
	return projectInstructionsPreamble + "\n\n<project_instructions source=\"" + snapshot.SourcePath + "\">\n" + content + "\n</project_instructions>"
}
