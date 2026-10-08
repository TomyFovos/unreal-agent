package claudecode

import "strings"

// Catalog metadata is not a capability attestation. Check the actual argv before
// auth/model execution so stream validation can rely on safe-mode and explicitly
// disabled tools/MCP. No caller-supplied argv or prompt values enter errors.
func validateLaunchContract(args []string) error {
	for _, required := range []struct {
		flag, value string
		argument    bool
	}{
		{flag: "--safe-mode"},
		{flag: "--restricted"},
		{flag: "--setting-sources", value: "", argument: true},
		{flag: "--settings", value: settings, argument: true},
		{flag: "--tools", value: "", argument: true},
		{flag: "--disallowedTools", value: "*", argument: true},
		{flag: "--strict-mcp-config"},
		{flag: "--mcp-config", value: `{"mcpServers":{}}`, argument: true},
		{flag: "--disable-slash-commands"},
		{flag: "--no-chrome"},
	} {
		count := 0
		for i, arg := range args {
			if strings.HasPrefix(arg, required.flag+"=") {
				return &Error{Code: "isolation_contract_invalid"}
			}
			if arg == required.flag {
				count++
				if required.argument && (i+1 >= len(args) || args[i+1] != required.value) {
					return &Error{Code: "isolation_contract_invalid"}
				}
			}
		}
		if count != 1 {
			return &Error{Code: "isolation_contract_invalid"}
		}
	}
	for _, arg := range args {
		if arg == "--bare" || strings.HasPrefix(arg, "--bare=") {
			return &Error{Code: "isolation_contract_invalid"}
		}
	}
	return nil
}
