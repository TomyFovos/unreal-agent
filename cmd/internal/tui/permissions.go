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
	if s.Permissions.Stopped {
		p.Title = "Permissions expired · Session stopped"
		p.Options = nil
		p.Hint = "Resume starts Read Only; privileged operations require reapproval"
		return p
	}
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
		"Continues after detach; Session stop/resume and Host restart require reapproval. /permissions can revoke. Operator maximum and explicit denies still apply.",
	}}
}

// operationPermissionConfirmation shows what will run first and starts on
// Allow once; Deny is one step down.
func operationPermissionConfirmation(s Snapshot, op permission.Pending) *Picker {
	p := &Picker{Kind: "permission-once", Title: "Allow " + toolLabel(SafeText(op.Tool)) + "?", Options: []string{"Allow once", "Deny"}, PermissionRevision: s.Permissions.Revision, PermissionOperation: op.ID, Instructions: []string{SafeText(op.Scope)}}
	if op.Tool == "Bash" {
		p.Footer = append(p.Footer, "Runs as you, with your files, network and credentials; no OS sandbox.")
	}
	p.Footer = append(p.Footer,
		"Esc: answer later in /permissions. Timeout or detach denies.",
		"Session "+SafeText(string(s.ID))+" · request "+SafeText(op.ID))
	return p
}

func toolLabel(name string) string {
	if name != "" && name[0] >= 'a' && name[0] <= 'z' {
		return string(name[0]-'a'+'A') + name[1:]
	}
	return name
}

// permissionGrants reports whether confirming the picker's selection grants.
func permissionGrants(p *Picker) bool {
	switch p.Kind {
	case "permission-once":
		return p.Selection == 0
	case "permission-full":
		return p.Selection == 1
	}
	return false
}

func permissionControlAction(p *Picker) permission.ControlRequest {
	r := permission.ControlRequest{Expected: p.PermissionRevision}
	if p.Kind == "permission-once" {
		r.Action = "decide"
		r.Operation = p.PermissionOperation
		r.Allow = permissionGrants(p)
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
	if s.Permissions.Stopped {
		return "Permissions: expired · Session stopped (selected " + s.Permissions.SelectedMode.Label() + ")"
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
	if s.Permissions.Stopped {
		return "EXPIRED "
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
