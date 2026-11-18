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

// ResolveLayer keeps worker and collector selections below a lead. Those
// layers carry that ancestry to further descendants, regardless of depth.
func ResolveLayer(template, parentLayer string) string {
	role := RoleGroup(template)
	if role == "worker" || role == "collector" {
		switch parentLayer {
		case "lead", "worker", "collector":
			return role
		default:
			return "lead"
		}
	}
	return role
}
