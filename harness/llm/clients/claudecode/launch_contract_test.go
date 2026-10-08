package claudecode

import (
	"slices"
	"strings"
	"testing"
)

func TestLaunchContractRequiresIsolation(t *testing.T) {
	for _, suffix := range [][]string{
		{"auth", "status"},
		{"-p", "--no-session-persistence", "--permission-prompts", "none", "--max-turns", "1"},
	} {
		args := append(isolationArgs(), suffix...)
		if err := validateLaunchContract(args); err != nil {
			t.Fatal("production launch contract was rejected", err)
		}
		for _, flag := range []string{"--safe-mode", "--restricted", "--setting-sources", "--settings", "--tools", "--disallowedTools", "--strict-mcp-config", "--mcp-config", "--disable-slash-commands", "--no-chrome"} {
			t.Run(flag, func(t *testing.T) {
				i := slices.Index(args, flag)
				without := slices.Delete(slices.Clone(args), i, i+1)
				requireCode(t, validateLaunchContract(without), "isolation_contract_invalid")
				duplicate := append(slices.Clone(args), flag)
				requireCode(t, validateLaunchContract(duplicate), "isolation_contract_invalid")
			})
		}
	}
}

func TestLaunchContractRejectsOverridesWithoutExposingValues(t *testing.T) {
	for _, flag := range []string{"--setting-sources", "--settings", "--tools", "--disallowedTools", "--mcp-config"} {
		t.Run(flag, func(t *testing.T) {
			args := isolationArgs()
			args[slices.Index(args, flag)+1] = "override-sensitive"
			err := validateLaunchContract(args)
			requireCode(t, err, "isolation_contract_invalid")
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatal("launch failure retained a raw argument")
			}
		})
	}
	for _, flag := range []string{"--safe-mode=false", "--tools=Bash", `--mcp-config={"mcpServers":{"private-sensitive":{}}}`, "--bare", "--bare=true"} {
		err := validateLaunchContract(append(isolationArgs(), flag))
		requireCode(t, err, "isolation_contract_invalid")
		if strings.Contains(err.Error(), "sensitive") {
			t.Fatal("launch failure retained a raw argument")
		}
	}
	for _, flag := range []string{"--tools", "--disallowedTools", "--mcp-config"} {
		args := isolationArgs()
		i := slices.Index(args, flag)
		args = append(slices.Delete(args, i, i+2), flag)
		requireCode(t, validateLaunchContract(args), "isolation_contract_invalid")
	}
}
