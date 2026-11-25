package subagent

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

// BriefFields are the optional facts supplied by the delegating agent.
// Text (including any paths and commands) is kept as supplied.
type BriefFields struct {
	BudgetSteps        int    `json:"budget_steps,omitzero"`
	Title              string `json:"title,omitzero"`
	Purpose            string `json:"purpose,omitzero"`
	Inputs             string `json:"inputs,omitzero"`
	Outputs            string `json:"outputs,omitzero"`
	Verification       string `json:"verification,omitzero"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitzero"`
}

// DelegationBrief records the launch conditions once. Neither displaying nor
// restating a field authorizes an operation or changes a live permission mode.
type DelegationBrief struct {
	Fields          BriefFields
	Root            string
	WriteScope      string
	TMPDIR          string
	Prohibitions    string
	ReportFormat    string
	CertaintyLabels string
	NextStep        string
}

const unspecified = "未指定/unspecified"

func newBrief(child ChildConfig, fields BriefFields, cwd, tmpdir string, mode permission.Mode) DelegationBrief {
	root := filepath.Clean(child.Workspace)
	if cwd != "" && filepath.Clean(cwd) != root {
		root += "\nProcess cwd: " + cwd
	}
	if tmpdir == "" {
		tmpdir = "process default"
	}

	policy := child.Policy
	writeScope := "No write roots configured"
	if policy.FilesystemUnrestricted {
		writeScope = "Unrestricted filesystem"
	} else if len(policy.WriteRoots) > 0 {
		writeScope = "Configured write roots: " + strings.Join(policy.WriteRoots, ", ")
	}
	var prohibitions []string
	switch mode {
	case permission.ReadOnly:
		prohibitions = append(prohibitions, "Read Only mode denies write operations.")
		writeScope = "Writes denied in Read Only mode. " + writeScope
	case permission.Ask:
		writeScope = "Writes require parent approval in Ask for Approval mode. " + writeScope
	}
	if child.TextOnly {
		prohibitions = append(prohibitions, "The selected runtime is text-only.")
	}
	if !policy.FilesystemUnrestricted {
		if len(policy.ReadRoots) == 0 {
			prohibitions = append(prohibitions, "No read roots configured.")
		} else {
			prohibitions = append(prohibitions, "Configured read roots: "+strings.Join(policy.ReadRoots, ", "))
		}
	}
	if !policy.NetworkUnrestricted {
		if len(policy.NetworkOrigins) == 0 {
			prohibitions = append(prohibitions, "No network origins configured.")
		} else {
			prohibitions = append(prohibitions, "Configured network origins: "+strings.Join(policy.NetworkOrigins, ", "))
		}
	}
	if policy.ProcessMode != permission.ProcessUnrestricted || !policy.FilesystemUnrestricted || !policy.NetworkUnrestricted {
		prohibitions = append(prohibitions, "The configured policy does not grant unrestricted subprocess execution.")
	}
	var runtime struct{ DisallowedTools []string }
	if json.Unmarshal(child.Runtime, &runtime) == nil && len(runtime.DisallowedTools) > 0 {
		prohibitions = append(prohibitions, "Disabled tools: "+strings.Join(runtime.DisallowedTools, ", "))
	}
	if source := child.ProjectInstructions; source != nil && source.SourceKind == projectinstructions.SourceWorkspaceAgents {
		prohibitions = append(prohibitions, "Bound workspace instructions: "+filepath.Join(child.Workspace, source.SourcePath))
	}
	for _, text := range []string{child.Task, fields.AcceptanceCriteria} {
		for line := range strings.SplitSeq(text, "\n") {
			for _, prefix := range []string{"prohibitions: ", "Prohibitions: ", "禁止事項: ", "禁止事項："} {
				if value, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(value) != "" {
					prohibitions = append(prohibitions, value)
					break
				}
			}
		}
	}
	if len(prohibitions) == 0 {
		prohibitions = append(prohibitions, "No additional prohibitions specified in this brief.")
	}
	return DelegationBrief{
		Fields: fields, Root: root, WriteScope: mode.Label() + "\n" + writeScope, TMPDIR: tmpdir,
		Prohibitions:    strings.Join(prohibitions, "\n"),
		ReportFormat:    "Status, artifact paths, measured verification (command and result), open issues, and next step. Include evidence with numbers and source locators when applicable. Identify missing deliverables and verification that failed or was not run.",
		CertaintyLabels: "Mark claims confirmed, estimated, or unknown and give their source. Fill in the labels when reporting.",
		NextStep:        "Name the next action and its owner, or write none.",
	}
}

// childTMPDIR uses the environment of the launched process. The existing
// launcher inherits the ambient environment when no entries are provided.
func childTMPDIR(environment []string) string {
	for i := len(environment) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(environment[i], "TMPDIR="); ok {
			return value
		}
	}
	if len(environment) == 0 {
		return os.Getenv("TMPDIR")
	}
	return ""
}

type briefField struct{ key, rule, value string }

func (b DelegationBrief) fields() []briefField {
	fields := []briefField{
		{"root", "Q1", b.Root},
		{"write_scope", "Q2", b.WriteScope},
		{"tmpdir", "Q3", b.TMPDIR},
		{"prohibitions", "Q4", b.Prohibitions},
		{"report_format", "Q5", b.ReportFormat},
		{"certainty_labels", "Q6", b.CertaintyLabels},
		{"next_step", "Q6", b.NextStep},
		{"title", "Q6", b.Fields.Title},
		{"purpose", "Q6", b.Fields.Purpose},
		{"inputs", "Q6", b.Fields.Inputs},
		{"outputs", "Q6", b.Fields.Outputs},
		{"verification", "Q6", b.Fields.Verification},
		{"acceptance_criteria", "Q6", b.Fields.AcceptanceCriteria},
	}
	if b.Fields.BudgetSteps > 0 {
		fields = append(fields, briefField{"budget_steps", "Q6", fmt.Sprintf("%d model calls across this Session and all its children. Tell your parent before exceeding it.", b.Fields.BudgetSteps)})
	}
	return fields
}

// Render is used by the child's initial input and by the parent's preview.
func (b DelegationBrief) Render(task string) string {
	var text strings.Builder
	text.WriteString("Delegation brief v1\nLaunch conditions reflect the start. Operations use the current permission settings. Relative paths in this brief use the work root.\n")
	for _, field := range b.fields() {
		value := field.value
		if strings.TrimSpace(value) == "" {
			value = unspecified
		}
		text.WriteString("\n" + field.key + ": " + value + "\n")
	}
	text.WriteString("\nTo request one stored field, send exactly 'brief <field>' with SendParent.\n")
	if task != "" {
		text.WriteString("\nTask instructions:\n" + task)
	}
	return text.String()
}

// ParentMessageReceipt records an automatic reply. The answer is kept outside
// bounded tool output.
type ParentMessageReceipt struct {
	RuleID  string `json:"rule_id"`
	Locator string `json:"locator,omitzero"`
	Answer  string `json:"answer,omitzero"`
}

func (c ChildConfig) routeQuestion(text string) ParentMessageReceipt {
	key, ok := strings.CutPrefix(text, "brief ")
	if !ok || strings.ContainsAny(key, " \t\r\n") {
		return ParentMessageReceipt{RuleID: "Q7"}
	}
	if c.Brief != nil {
		for _, field := range c.Brief.fields() {
			if field.key != key {
				continue
			}
			if strings.TrimSpace(field.value) == "" {
				return ParentMessageReceipt{RuleID: "X1"}
			}
			locator := "session:" + string(c.ParentID) + "/operation:" + string(c.OperationID) + "/brief/v1/" + key
			return ParentMessageReceipt{RuleID: field.rule, Locator: locator, Answer: field.value}
		}
	}
	return ParentMessageReceipt{RuleID: "X1"}
}
