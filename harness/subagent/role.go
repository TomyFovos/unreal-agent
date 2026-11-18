package subagent

import "strings"

// RoleGroup is shared by role selection and historical layer attribution.
// Custom templates keep an unknown layer rather than guessing from a model.
func RoleGroup(template string) string {
	if template == "worker" || strings.HasPrefix(template, "worker-") {
		return "worker"
	}
	switch template {
	case "lead", "collector":
		return template
	default:
		return "unknown"
	}
}
