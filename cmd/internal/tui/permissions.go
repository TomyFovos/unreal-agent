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
	m.state.Version++
}
func permissionPicker(s Snapshot) *Picker {
	p := &Picker{Kind: "permissions", Title: "Permissions · " + s.Permissions.Mode.Label(), Hint: "権限を選んでください", PermissionRevision: s.Permissions.Revision, Options: []string{"Read Only · 読むだけ", "Ask · 書き込みと実行の前に確認", "Full Access · 確認なし。OS の隔離ではありません"}}
	if s.Permissions.Stopped {
		p.Title = "Permissions · 停止中"
		p.Options = nil
		p.Hint = "/resume で再開すると、前に選んだ権限を使います"
		return p
	}
	if !s.Permissions.Enabled {
		p.Options = nil
		p.Hint = "この設定では権限を変更できません。権限を選べる設定で起動してください"
		return p
	}
	switch s.Permissions.Mode {
	case permission.Ask:
		p.Selection = 1
	case permission.FullAccess:
		p.Selection = 2
	}
	for _, op := range s.Permissions.Pending {
		p.Options = append(p.Options, "確認: "+SafeText(op.Tool)+" · "+SafeText(op.Scope))
	}
	return p
}

// operationPermissionConfirmation shows what will run first and starts on
// Allow once; Deny is one step down.
func operationPermissionConfirmation(s Snapshot, op permission.Pending) *Picker {
	p := &Picker{Kind: "permission-once", Title: "Allow " + toolLabel(SafeText(op.Tool)) + "?", Options: []string{"Allow once", "Deny"}, PermissionRevision: s.Permissions.Revision, PermissionOperation: op.ID, Instructions: []string{SafeText(op.Scope)}}
	if op.Tool == "Bash" {
		p.Footer = append(p.Footer, "Runs as you, with your files, network and credentials; no OS sandbox.")
	}
	p.Footer = append(p.Footer,
		"Esc: answer later in /permissions. /stop cancels the operation.")
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
	return r
}

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
