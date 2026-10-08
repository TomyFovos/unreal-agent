package claudecode

// SDKControlListPermissionRulesResponse reports the CLI's live evaluation, not
// Unreal's interpretation of a managed-settings file. In 2.1.285 managedOnly
// removes cliArg/session allow rules while retaining denies. Consequently SDK
// registration and --allowedTools argv alone do not prove an active allowlist.
// This read-only gate runs in the SAME process before user input. It neither
// changes policy nor answers a permission prompt; later policy refresh/denials
// still fail closed through permissionDeniedEvent.
type bridgePermissionState struct {
	ManagedOnly *bool                  `json:"managedOnly"`
	Rules       []bridgePermissionRule `json:"rules"`
}

type bridgePermissionRule struct {
	Behavior    string `json:"behavior"`
	Source      string `json:"source"`
	Rule        string `json:"rule"`
	NotInEffect bool   `json:"notInEffect"`
}

func validateBridgePermissions(state *bridgePermissionState, names map[string]bridgeTool) error {
	if state == nil || state.ManagedOnly == nil || state.Rules == nil || len(state.Rules) > 4096 {
		return &Error{Code: "bridge_unavailable"}
	}
	allowed := make(map[string]bool, len(names))
	for _, rule := range state.Rules {
		if rule.Behavior != "allow" || rule.NotInEffect {
			continue
		}
		if *state.ManagedOnly && rule.Source != "policySettings" {
			continue
		}
		switch rule.Source {
		case "cliArg", "policySettings":
			// Require the generated EXACT bare name, never wildcard grants,
			// unknown sources, prompts or policy-rule strings interpreted here.
			if _, owned := names[rule.Rule]; owned {
				allowed[rule.Rule] = true
			}
		}
	}
	if len(allowed) != len(names) {
		return &Error{Code: "bridge_allowlist_inactive", managedOnly: *state.ManagedOnly, allowedTools: len(allowed), expectedTools: len(names)}
	}
	return nil
}
