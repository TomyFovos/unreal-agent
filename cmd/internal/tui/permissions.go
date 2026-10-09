package tui

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func (m *Model) setPermissions(generation string, snapshot permission.ModeSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Generation != generation || snapshot.Revision < m.state.Permissions.Revision {
		return
	}
	snapshot.Pending = append([]permission.Pending(nil), snapshot.Pending...)
	m.state.Permissions = snapshot
}
func permissionPicker(s Snapshot) *Picker {
	p := &Picker{Kind: "permissions", Title: "Permissions · " + s.Permissions.Mode.Label(), Hint: "Schemas are capabilities, not permission grants", PermissionRevision: s.Permissions.Revision, Options: []string{"Read Only", "Ask for Approval", "Full Access"}}
	if !s.Permissions.Enabled {
		p.Options = nil
		p.Hint = "Legacy configured policy; opt in with PermissionModes.Enabled and a reviewed Maximum"
		return p
	}
	switch s.Permissions.Mode {
	case permission.Ask:
		p.Selection = 1
	case permission.FullAccess:
		p.Selection = 2
	}
	for _, op := range s.Permissions.Pending {
		p.Options = append(p.Options, "Review "+SafeText(op.Tool)+" · Operation "+SafeText(op.ID))
	}
	return p
}
func fullPermissionConfirmation(s Snapshot, revision uint64, challenge string) *Picker {
	return &Picker{Kind: "permission-full", Title: "Confirm Full Access · Session " + SafeText(string(s.ID)), Options: []string{"Deny / keep current mode", "Confirm Full Access for this Session"}, PermissionRevision: revision, PermissionChallenge: challenge, Instructions: []string{
		"NO OS SANDBOX. Bash can execute arbitrary commands.",
		"Filesystem, network, credentials, environment and shared mounts may be accessed.",
		"Continues after detach; Host restart requires reapproval. /permissions can revoke. Operator maximum and explicit denies still apply.",
	}}
}
func operationPermissionConfirmation(s Snapshot, op permission.Pending) *Picker {
	return &Picker{Kind: "permission-once", Title: "Session " + SafeText(string(s.ID)) + " · " + SafeText(op.Tool), Options: []string{"Deny this Operation", "Allow Once (this Operation only)"}, PermissionRevision: s.Permissions.Revision, PermissionOperation: op.ID, Instructions: []string{
		"Operation: " + SafeText(op.ID),
		"Scope: " + SafeText(op.Scope),
		"Unanswered, timeout, cancellation and detach deny. No automatic approval.",
	}}
}
func permissionControlAction(p *Picker) permission.ControlRequest {
	r := permission.ControlRequest{Expected: p.PermissionRevision}
	if p.Kind == "permission-once" {
		r.Action = "decide"
		r.Operation = p.PermissionOperation
		r.Allow = p.Selection == 1
	}
	if p.Kind == "permission-full" {
		r.Action = "set_mode"
		r.Mode = permission.FullAccess
		r.Challenge = p.PermissionChallenge
	}
	return r
}

// Approval requires a reviewable display; Tiny keeps its normal editing behavior
// but cannot hide access warnings behind an active confirm key.
func permissionReviewFits(width, height int) bool { return width >= 80 && height >= 24 }
func permissionStatus(s Snapshot) string {
	if !s.Permissions.Enabled {
		return ""
	}
	return fmt.Sprintf("Permissions: %s", s.Permissions.Mode.Label())
}

func permissionSeparator(label string) string {
	if label != "" {
		return "  "
	}
	return ""
}
func permissionBadge(s Snapshot) string {
	if !s.Permissions.Enabled {
		return ""
	}
	switch s.Permissions.Mode {
	case permission.ReadOnly:
		return "RO "
	case permission.Ask:
		return "ASK "
	case permission.FullAccess:
		return "FULL "
	}
	return "? "
}
